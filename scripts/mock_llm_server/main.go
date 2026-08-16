// Command mock_llm_server 是一个 OpenAI 兼容的模拟上游，用于在本地验证
// new-api 的限速排队 / 熔断粒度 / 恢复计时 / key 轮换，禁止拿真实上游做效果测试。
//
// 支持的行为（按需组合）：
//   - /v1/models             返回模型列表
//   - /v1/chat/completions   支持 stream 与非 stream 的假回复
//   - -limit-model + -limit-success N：某模型前 N 次成功、之后一直 429（可带 Retry-After）
//   - -inject-500-every M：每 M 个请求返回一次 500
//   - -delay D：每个请求先睡 D 毫秒
//
// 运行：go run ./scripts/mock_llm_server -addr :8090 -limit-model agnes-2.5-flash -limit-success 3
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	addr           string
	models         []string
	limitModel     string // 限流模型名，"all" 表示全部模型
	limitSuccess   int    // 该模型前 N 次成功，之后 429
	limitWindow    int    // 限流计数窗口（秒），超过则重置计数；0=不清零
	retryAfter     bool
	inject500Every int // 每 M 个请求返回 500，0=关闭
	delayMS        int
}

type modelCounter struct {
	count       int
	windowStart time.Time
}

var (
	cfg      config
	mu       sync.Mutex
	counters = map[string]*modelCounter{} // model -> 限流计数
	global   atomic.Int64                 // 用于 inject500 计数
)

func main() {
	flag.StringVar(&cfg.addr, "addr", ":8090", "监听地址")
	modelsFlag := flag.String("models", "gpt-3.5-turbo,agnes-2.5-flash", "逗号分隔的模型列表")
	flag.StringVar(&cfg.limitModel, "limit-model", "", "要限流的模型名（all=全部），空=不限流")
	flag.IntVar(&cfg.limitSuccess, "limit-success", 3, "限流模型前 N 次成功，之后返回 429")
	flag.IntVar(&cfg.limitWindow, "limit-window", 0, "限流计数窗口（秒），窗口内累计 N 次后 429，窗口到期重置；0=不清零")
	flag.BoolVar(&cfg.retryAfter, "retry-after", true, "429 时是否带 Retry-After 头")
	flag.IntVar(&cfg.inject500Every, "inject-500-every", 0, "每 M 个请求返回一次 500，0=关闭")
	flag.IntVar(&cfg.delayMS, "delay", 0, "每个请求的延迟（毫秒）")
	flag.Parse()

	cfg.models = splitAndTrim(*modelsFlag)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)

	log.Printf("mock_llm_server listening on %s, models=%v, limit-model=%s, limit-success=%d",
		cfg.addr, cfg.models, cfg.limitModel, cfg.limitSuccess)
	if err := http.ListenAndServe(cfg.addr, mux); err != nil {
		log.Fatal(err)
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	type modelObj struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	data := make([]modelObj, 0, len(cfg.models))
	for _, m := range cfg.models {
		data = append(data, modelObj{ID: m, Object: "model"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if cfg.delayMS > 0 {
		time.Sleep(time.Duration(cfg.delayMS) * time.Millisecond)
	}

	// 注入 5xx
	if cfg.inject500Every > 0 && global.Add(1)%int64(cfg.inject500Every) == 0 {
		writeError(w, http.StatusInternalServerError, "injected internal error", "server_error")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request", "invalid_request_error")
		return
	}
	if req.Model == "" {
		req.Model = cfg.models[0]
	}

	// 限流逻辑：仅对目标模型生效
	if isLimitedModel(req.Model) {
		n := incr(req.Model)
		if n > cfg.limitSuccess {
			code := http.StatusTooManyRequests
			if cfg.retryAfter {
				w.Header().Set("Retry-After", "5")
			}
			writeError(w, code, fmt.Sprintf("model %s rate limited (request #%d)", req.Model, n), "rate_limit_error")
			return
		}
	}

	if req.Stream {
		handleStream(w, req.Model)
		return
	}
	handleNonStream(w, req.Model)
}

func isLimitedModel(model string) bool {
	if cfg.limitModel == "" {
		return false
	}
	if cfg.limitModel == "all" {
		return true
	}
	return model == cfg.limitModel
}

func incr(model string) int {
	mu.Lock()
	defer mu.Unlock()
	c, ok := counters[model]
	if !ok {
		c = &modelCounter{windowStart: time.Now()}
		counters[model] = c
	}
	if cfg.limitWindow > 0 && time.Since(c.windowStart) >= time.Duration(cfg.limitWindow)*time.Second {
		c.count = 0
		c.windowStart = time.Now()
	}
	c.count++
	return c.count
}

func handleNonStream(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "mock reply from " + model},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	writeJSON(w, http.StatusOK, resp)
}

func handleStream(w http.ResponseWriter, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(obj any) {
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	send(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{"content": "mock"}, "finish_reason": nil},
		},
	})
	send(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
		},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSONRaw(w, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSONRaw(w, v)
}

func writeJSONRaw(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}
