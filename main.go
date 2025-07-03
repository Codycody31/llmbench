package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"
)

type usageBlock struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type successResp struct {
	Usage usageBlock `json:"usage"`
}

type errorResp struct {
	Error string `json:"error"`
}

type ollamaResp struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type runMetrics struct {
	Run              int     `json:"run"`
	Model            string  `json:"model"`
	Stream           bool    `json:"stream"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	LatencyMs        float64 `json:"latency_ms"`
	TokPerSec        float64 `json:"tok_per_sec"`
}

func (rm runMetrics) ToMap() map[string]any {
	return map[string]any{
		"run":               rm.Run,
		"model":             rm.Model,
		"stream":            rm.Stream,
		"prompt_tokens":     rm.PromptTokens,
		"completion_tokens": rm.CompletionTokens,
		"total_tokens":      rm.TotalTokens,
		"latency_ms":        rm.LatencyMs,
		"tok_per_sec":       rm.TokPerSec,
	}
}

type logFields map[string]any

func storeRunData(dataDir string, run int, dataType string, content string) (error, string) {
	filename := fmt.Sprintf("%s/%03d.%s.txt", dataDir, run, dataType)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("error creating directory %s: %w", dataDir, err), filename
	}
	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		return fmt.Errorf("error writing %s: %w", filename, err), filename
	}
	return nil, filename
}

func logEvent(run int, event string, fields logFields) {
	parts := make([]string, 0, len(fields)+2)
	parts = append(parts, fmt.Sprintf("Run %03d", run), event)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	log.Println(strings.Join(parts, " | "))
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func countTokens(text string) int {
	return len(strings.Fields(text))
}

func callAPI(
	ctx context.Context,
	run int,
	client *http.Client,
	baseURL, key, model, prompt string,
	maxTokens int,
	style string,
	stream bool,
	ch chan<- runMetrics,
	wg *sync.WaitGroup,
	dataDir string,
	storeData bool,
) {
	defer wg.Done()

	var endpoint string
	var body []byte

	switch style {
	case "ollama":
		endpoint = strings.TrimRight(baseURL, "/") + "/chat"
		body, _ = json.Marshal(map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
			"stream":   stream,
		})
	default:
		endpoint = strings.TrimRight(baseURL, "/") + "/chat/completions"
		body, _ = json.Marshal(map[string]any{
			"model":       model,
			"messages":    []map[string]string{{"role": "user", "content": prompt}},
			"temperature": 0.7,
			"max_tokens":  maxTokens,
			"stream":      stream,
		})
	}

	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if style != "ollama" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	promptTokens := countTokens(prompt)
	logEvent(run, "request", logFields{"model": model, "stream": stream, "prompt_tokens": promptTokens})

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		logEvent(run, "error", logFields{"type": "transport", "error": err.Error()})
		return
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		logEvent(run, "error", logFields{"type": "http", "status_code": resp.StatusCode, "response": strings.TrimSpace(string(raw))})
		return
	}

	if stream {
		reader := bufio.NewReader(resp.Body)
		logEvent(run, "stream-start", logFields{"model": model})

		var contentBuilder strings.Builder

		type ollamaMeta struct {
			Model              string `json:"model"`
			CreatedAt          string `json:"created_at"`
			DoneReason         string `json:"done_reason"`
			TotalDuration      int64  `json:"total_duration"`
			LoadDuration       int64  `json:"load_duration"`
			PromptEvalCount    int    `json:"prompt_eval_count"`
			PromptEvalDuration int64  `json:"prompt_eval_duration"`
			EvalCount          int    `json:"eval_count"`
			EvalDuration       int64  `json:"eval_duration"`
		}
		var meta ollamaMeta

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if style == "ollama" && strings.Contains(line, "\"done_reason\"") {
				_ = json.Unmarshal([]byte(line), &meta)
				break
			}

			var chunk map[string]any
			if err := json.Unmarshal([]byte(line), &chunk); err == nil {
				if msg, ok := chunk["message"].(map[string]any); ok {
					if cstr, ok2 := msg["content"].(string); ok2 {
						contentBuilder.WriteString(cstr)
						if storeData {
							err, _ := storeRunData(dataDir, run, "response", contentBuilder.String())
							if err != nil {
								logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
							}
						}
					}
				}
			}
		}

		elapsedStream := time.Since(start)

		pTok := promptTokens
		if style == "ollama" {
			pTok = meta.PromptEvalCount
		}

		runMetrics := runMetrics{
			Run:              run,
			Model:            model,
			Stream:           stream,
			PromptTokens:     pTok,
			CompletionTokens: countTokens(contentBuilder.String()),
			TotalTokens:      countTokens(contentBuilder.String()),
			LatencyMs:        elapsedStream.Seconds() * 1e3,
			TokPerSec:        float64(countTokens(contentBuilder.String())) / elapsedStream.Seconds(),
		}

		logEvent(run, "success", runMetrics.ToMap())

		ch <- runMetrics

		if storeData {
			err, filename := storeRunData(dataDir, run, "response", contentBuilder.String())
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "response-stored", logFields{"file": filename})
			data, err := json.Marshal(runMetrics)
			if err != nil {
				logEvent(run, "error", logFields{"type": "json_marshal", "error": err.Error()})
			}
			err, filename = storeRunData(dataDir, run, "metrics", string(data))
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "metrics-stored", logFields{"file": filename})
		}

		return
	}

	raw, _ := io.ReadAll(resp.Body)
	if i := bytes.IndexByte(raw, '{'); i >= 0 {
		raw = raw[i:]
	}

	var metrics runMetrics

	if style == "ollama" {
		var or ollamaResp
		if err := json.Unmarshal(raw, &or); err != nil {
			logEvent(run, "error", logFields{"type": "json_parse", "error": err.Error()})
			return
		}

		metrics = runMetrics{
			Run:              run,
			Model:            model,
			Stream:           stream,
			PromptTokens:     promptTokens,
			CompletionTokens: countTokens(or.Message.Content),
			TotalTokens:      countTokens(or.Message.Content),
			LatencyMs:        elapsed.Seconds() * 1e3,
			TokPerSec:        float64(countTokens(or.Message.Content)) / elapsed.Seconds(),
		}
		logEvent(run, "success", metrics.ToMap())
		if storeData {
			err, filename := storeRunData(dataDir, run, "response", or.Message.Content)
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "response-stored", logFields{"file": filename})
			data, err := json.Marshal(metrics)
			if err != nil {
				logEvent(run, "error", logFields{"type": "json_marshal", "error": err.Error()})
			}
			err, filename = storeRunData(dataDir, run, "metrics", string(data))
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "metrics-stored", logFields{"file": filename})
		}
	} else {
		var ok successResp
		if err := json.Unmarshal(raw, &ok); err != nil {
			var apiErr errorResp
			if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
				logEvent(run, "error", logFields{"type": "api", "error": apiErr.Error})
			} else {
				logEvent(run, "error", logFields{"type": "json_parse", "error": err.Error()})
			}
			return
		}
		metrics = runMetrics{
			Run:              run,
			Model:            model,
			Stream:           stream,
			PromptTokens:     promptTokens,
			CompletionTokens: ok.Usage.CompletionTokens,
			TotalTokens:      ok.Usage.TotalTokens,
			LatencyMs:        elapsed.Seconds() * 1e3,
			TokPerSec:        float64(ok.Usage.TotalTokens) / elapsed.Seconds(),
		}
		logEvent(run, "success", metrics.ToMap())
		if storeData {
			err, filename := storeRunData(dataDir, run, "response", string(raw))
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "response-stored", logFields{"file": filename})
			data, err := json.Marshal(metrics)
			if err != nil {
				logEvent(run, "error", logFields{"type": "json_marshal", "error": err.Error()})
			}
			err, filename = storeRunData(dataDir, run, "metrics", string(data))
			if err != nil {
				logEvent(run, "error", logFields{"type": "store_data", "error": err.Error()})
			}
			logEvent(run, "metrics-stored", logFields{"file": filename})
		}
	}

	ch <- metrics
}

func main() {
	app := &cli.App{
		Name:  "llmbench",
		Usage: "tiny load-tester for OpenAI & Ollama like chat APIs",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "base-url", Value: "https://api.openai.com/v1", Usage: "API base URL"},
			&cli.StringFlag{Name: "key", EnvVars: []string{"LLM_API_KEY"}, Usage: "Bearer token (not used by Ollama)"},
			&cli.StringFlag{Name: "style", Value: "openai", Usage: "API style: openai or ollama"},
			&cli.BoolFlag{Name: "stream", Usage: "enable streaming (SSE) mode"},
			&cli.IntFlag{Name: "runs", Value: 100, Usage: "total requests to send"},
			&cli.IntFlag{Name: "concurrency", Value: 0, Usage: "simultaneous requests (0 = runs)"},
			&cli.IntFlag{Name: "max-tokens", Value: 4096, Usage: "max_tokens per request (OpenAI only)"},
			&cli.StringFlag{Name: "model", Value: "gpt-4o-mini", Usage: "model ID"},
			&cli.StringFlag{Name: "prompt", Value: "Explain the fundamental concepts of relativity in detail.", Usage: "user message"},
			&cli.DurationFlag{Name: "timeout", Value: 60 * time.Second, Usage: "HTTP timeout (ignored in streaming)"},
			&cli.BoolFlag{Name: "unload-model", Value: false, Usage: "unload model after all runs complete (Ollama only)"},
			&cli.StringFlag{Name: "data-dir", Value: "./runs", Usage: "directory to save data files"},
			&cli.BoolFlag{Name: "store-data", Value: false, Usage: "store data files (responses, metrics)"},
		},
		Action: func(c *cli.Context) error {
			style := strings.ToLower(c.String("style"))

			dataDir := c.String("data-dir")
			storeData := c.Bool("store-data")
			if storeData && dataDir == "" {
				return cli.Exit("data-dir must be set when store-data is enabled", 1)
			}

			apiKey := c.String("key")
			if style != "ollama" && apiKey == "" {
				return cli.Exit("missing API key (use --key or set LLM_API_KEY)", 1)
			}

			runs := c.Int("runs")
			conc := c.Int("concurrency")
			if conc <= 0 || conc > runs {
				conc = runs
			}

			var client *http.Client
			if c.Bool("stream") {
				client = &http.Client{Timeout: 0}
			} else {
				client = &http.Client{Timeout: c.Duration("timeout")}
			}

			results := make(chan runMetrics, runs)
			var wg sync.WaitGroup
			sem := make(chan struct{}, conc)

			for i := 1; i <= runs; i++ {
				wg.Add(1)
				sem <- struct{}{}
				go func(run int) {
					defer func() { <-sem }()
					callAPI(
						c.Context,
						run, client,
						c.String("base-url"), apiKey,
						c.String("model"), c.String("prompt"),
						c.Int("max-tokens"),
						style,
						c.Bool("stream"),
						results, &wg,
						dataDir, storeData,
					)
				}(i)
			}

			go func() {
				wg.Wait()
				close(results)
			}()

			var sumC, sumT int
			var sumTPS float64
			var good int
			for m := range results {
				sumC += m.CompletionTokens
				sumT += m.TotalTokens
				sumTPS += m.TokPerSec
				good++
			}

			fmt.Printf("\n=== Summary ===\n")
			fmt.Printf("Successful calls  : %d / %d\n", good, runs)
			if good > 0 {
				fmt.Printf("Avg completion tokens    : %.2f\n", float64(sumC)/float64(good))
				fmt.Printf("Avg total tokens         : %.2f\n", float64(sumT)/float64(good))
				fmt.Printf("Avg tokens / sec         : %.2f\n", sumTPS/float64(good))
				fmt.Printf("Total completion tokens  : %d\n", sumC)
				fmt.Printf("Total tokens             : %d\n", sumT)
			}

			if style == "ollama" && c.Bool("unload-model") {
				endpoint := strings.TrimRight(c.String("base-url"), "/") + "/chat"
				body, _ := json.Marshal(map[string]any{
					"model":      c.String("model"),
					"keep_alive": 0,
				})
				req, _ := http.NewRequestWithContext(c.Context, "POST", endpoint, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					return fmt.Errorf("error unloading model: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					raw, _ := io.ReadAll(resp.Body)
					return fmt.Errorf("error unloading model: %s (status code %d)", strings.TrimSpace(string(raw)), resp.StatusCode)
				}
			}

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
