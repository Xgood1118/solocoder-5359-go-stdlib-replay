package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"example.com/replay/internal/session"
)

type Config struct {
	SessionPath string
	OutputPath  string
	Loop        int
	FilterURL   string
	Method      string
	Binds       []VariableBind
	RPS         int
	Concurrency int
	Delay       time.Duration
	RandomDelay [2]time.Duration
	Offset      time.Duration
	DryRun      bool
}

type ReplayError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Cause   string `json:"cause,omitempty"`
}

type ResultEntry struct {
	OriginalRequest  *session.RequestRecord  `json:"original_request"`
	ReplayedRequest  *session.RequestRecord  `json:"replayed_request"`
	OriginalResponse *session.ResponseRecord `json:"original_response"`
	ReplayedResponse *session.ResponseRecord `json:"replayed_response,omitempty"`
	Status           string                  `json:"status"`
	DurationMs       int64                   `json:"duration_ms"`
	Error            *ReplayError            `json:"error,omitempty"`
}

type ReplayResult struct {
	Version  string        `json:"version"`
	Generated string       `json:"generated"`
	Entries  []*ResultEntry `json:"entries"`
}

func Run(cfg Config) (*ReplayResult, error) {
	s, err := session.Load(cfg.SessionPath)
	if err != nil {
		return nil, err
	}

	entries := filterEntries(s.Requests, cfg.FilterURL, cfg.Method)
	if len(entries) == 0 {
		return nil, fmt.Errorf("没有匹配的请求需要回放 (filter=%s, method=%s)", cfg.FilterURL, cfg.Method)
	}

	loops := cfg.Loop
	if loops <= 0 {
		loops = 1
	}

	result := &ReplayResult{
		Version:   "1.0",
		Generated: session.NowRFC3339(),
		Entries:   make([]*ResultEntry, 0, len(entries)*loops),
	}

	vs := NewVariableStore()
	for _, bind := range cfg.Binds {
		if bind.Source == "env" {
			_ = vs.ApplyBind(bind)
		}
	}

	var bucket *TokenBucket
	if cfg.RPS > 0 {
		bucket = NewTokenBucket(float64(cfg.RPS))
	}

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for loop := 0; loop < loops; loop++ {
		for i, entry := range entries {
			if bucket != nil {
				bucket.Wait()
			}

			if loop > 0 || i > 0 {
				applyDelay(cfg)
			}

			wg.Add(1)
			sem <- struct{}{}

			go func(entry *session.Entry, loop, idx int) {
				defer wg.Done()
				defer func() { <-sem }()

				re := executeEntry(entry, cfg, vs, loop, idx)
				mu.Lock()
				result.Entries = append(result.Entries, re)
				mu.Unlock()
			}(entry, loop, i)
		}
	}

	wg.Wait()

	if cfg.OutputPath != "" {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("序列化结果失败: %w", err)
		}
		if err := ioutil.WriteFile(cfg.OutputPath, data, 0644); err != nil {
			return nil, fmt.Errorf("写入结果文件 %s 失败: %w", cfg.OutputPath, err)
		}
	}

	return result, nil
}

func filterEntries(entries []*session.Entry, filterURL, method string) []*session.Entry {
	filtered := make([]*session.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Request == nil {
			continue
		}
		if filterURL != "" && !strings.Contains(e.Request.URL, filterURL) {
			continue
		}
		if method != "" && !strings.EqualFold(e.Request.Method, method) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

func applyDelay(cfg Config) {
	if cfg.Delay > 0 {
		time.Sleep(cfg.Delay)
		return
	}
	if cfg.RandomDelay[0] > 0 && cfg.RandomDelay[1] > cfg.RandomDelay[0] {
		min := cfg.RandomDelay[0]
		max := cfg.RandomDelay[1]
		d := min + time.Duration(rand.Int63n(int64(max-min)))
		time.Sleep(d)
	}
}

func executeEntry(entry *session.Entry, cfg Config, vs *VariableStore, loop, idx int) *ResultEntry {
	re := &ResultEntry{
		OriginalRequest:  entry.Request,
		OriginalResponse: entry.Response,
		Status:           "pending",
	}

	replayedReq := vs.ReplaceInRequest(entry.Request)
	re.ReplayedRequest = replayedReq

	if cfg.Offset != 0 {
		if ts, err := session.ParseTimestamp(replayedReq.Timestamp); err == nil {
			replayedReq.Timestamp = ts.Add(cfg.Offset).Format(time.RFC3339Nano)
		}
	}

	if cfg.DryRun {
		re.Status = "dry_run"
		fmt.Printf("[DRY RUN] [%d-%d] %s %s\n", loop+1, idx+1, replayedReq.Method, replayedReq.URL)
		return re
	}

	startTime := time.Now()
	resp, err := sendRequest(replayedReq)
	duration := time.Since(startTime)
	re.DurationMs = duration.Milliseconds()

	if err != nil {
		re.Status = "failed"
		re.Error = classifyError(err)
		fmt.Printf("[FAIL] [%d-%d] %s %s - %s\n", loop+1, idx+1, replayedReq.Method, replayedReq.URL, err.Error())
		return re
	}

	respBody, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	respContentType := resp.Header.Get("Content-Type")
	respRecord := &session.ResponseRecord{
		Status:    resp.StatusCode,
		Headers:   session.HeadersToMap(resp.Header),
		Body:      session.NewBodyContent(respBody, respContentType, 0),
		Timestamp: session.NowRFC3339(),
	}
	re.ReplayedResponse = respRecord
	re.Status = "success"

	vs.SetResponse(respRecord)
	for _, bind := range cfg.Binds {
		if bind.Source == "response" || bind.Source == "regex" {
			_ = vs.ApplyBind(bind)
		}
	}

	fmt.Printf("[OK]   [%d-%d] %s %s -> %d (%v)\n", loop+1, idx+1, replayedReq.Method, replayedReq.URL, resp.StatusCode, duration)
	return re
}

func sendRequest(req *session.RequestRecord) (*http.Response, error) {
	var bodyBytes []byte
	var err error
	if req.Body != nil {
		bodyBytes, err = req.Body.Bytes()
		if err != nil {
			return nil, fmt.Errorf("读取请求体失败: %w", err)
		}
	}

	httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("创建HTTP请求失败: %w", err)
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client.Do(httpReq)
}

func classifyError(err error) *ReplayError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"):
		return &ReplayError{Type: "timeout", Message: msg, Cause: "请求超时，检查网络连接或目标服务状态"}
	case strings.Contains(msg, "connection refused"):
		return &ReplayError{Type: "connection", Message: msg, Cause: "连接被拒绝，目标服务可能未启动或端口错误"}
	case strings.Contains(msg, "no such host"):
		return &ReplayError{Type: "dns", Message: msg, Cause: "DNS解析失败，检查域名是否正确"}
	case strings.Contains(msg, "TLS") || strings.Contains(msg, "tls"):
		return &ReplayError{Type: "tls", Message: msg, Cause: "TLS握手失败，检查证书配置"}
	default:
		return &ReplayError{Type: "unknown", Message: msg}
	}
}
