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

type metric struct {
	comp int
	tot  int
	tps  float64
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
	ch chan<- metric,
	wg *sync.WaitGroup,
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

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Run %03d │ transport error: %v", run, err)
		return
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		log.Printf("Run %03d │ HTTP %d │ %s", run, resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}

	if stream {
		reader := bufio.NewReader(resp.Body)
		fmt.Printf("Run %03d │ streaming...\n", run)

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
					}
				}
			}
		}

		fmt.Printf("Run %03d │ stream complete │ done_reason=%s │ total_duration=%d\n",
			run, meta.DoneReason, meta.TotalDuration)

		comp := countTokens(contentBuilder.String())
		ch <- metric{comp: comp, tot: comp, tps: float64(comp) / elapsed.Seconds()}
		return
	}

	raw, _ := io.ReadAll(resp.Body)
	if i := bytes.IndexByte(raw, '{'); i >= 0 {
		raw = raw[i:]
	}

	var comp, tot int
	var tps float64

	if style == "ollama" {
		var or ollamaResp
		if err := json.Unmarshal(raw, &or); err != nil {
			log.Printf("Run %03d │ JSON parse error: %v", run, err)
			return
		}
		comp = countTokens(or.Message.Content)
		tot = comp
		tps = float64(comp) / elapsed.Seconds()
		log.Printf("Run %03d │ %4.0f ms │ oltokens=%d │ approx tok/s=%.1f",
			run, elapsed.Seconds()*1e3, comp, tps)
	} else {
		var ok successResp
		if err := json.Unmarshal(raw, &ok); err != nil {
			var apiErr errorResp
			if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
				log.Printf("Run %03d │ API error: %s", run, apiErr.Error)
			} else {
				log.Printf("Run %03d │ JSON parse error: %v", run, err)
			}
			return
		}
		comp = ok.Usage.CompletionTokens
		tot = ok.Usage.TotalTokens
		tps = float64(comp) / elapsed.Seconds()
		log.Printf("Run %03d │ %4.0f ms │ completion=%d │ total=%d │ %.1f tok/s",
			run, elapsed.Seconds()*1e3, comp, tot, tps)
	}

	ch <- metric{comp: comp, tot: tot, tps: tps}
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
		},
		Action: func(c *cli.Context) error {
			style := strings.ToLower(c.String("style"))
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

			results := make(chan metric, runs)
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
				sumC += m.comp
				sumT += m.tot
				sumTPS += m.tps
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
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
