package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strings"
	"time"
)

const (
	Version      = 1
	SessionType  = "replay-session"
	TruncatedTag = "TRUNCATED"
)

type BodyContent struct {
	IsBinary bool   `json:"is_binary"`
	Content  string `json:"content"`
	Truncated bool  `json:"truncated,omitempty"`
}

type RequestRecord struct {
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Query      map[string]string `json:"query,omitempty"`
	Body       *BodyContent      `json:"body,omitempty"`
	Timestamp  string            `json:"timestamp"`
}

type ResponseRecord struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       *BodyContent      `json:"body,omitempty"`
	Timestamp  string            `json:"timestamp"`
}

type Entry struct {
	Request  *RequestRecord  `json:"request"`
	Response *ResponseRecord `json:"response"`
}

type Session struct {
	Version  int      `json:"version"`
	Type     string   `json:"type"`
	Requests []*Entry `json:"requests"`
}

func NewSession() *Session {
	return &Session{
		Version:  Version,
		Type:     SessionType,
		Requests: make([]*Entry, 0),
	}
}

func NewBodyContent(data []byte, contentType string, maxSize int64) *BodyContent {
	if len(data) == 0 {
		return nil
	}

	bc := &BodyContent{}
	isBinary := !isTextContent(contentType)
	bc.IsBinary = isBinary

	if maxSize > 0 && int64(len(data)) > maxSize {
		data = data[:maxSize]
		bc.Truncated = true
	}

	if isBinary {
		bc.Content = base64.StdEncoding.EncodeToString(data)
	} else {
		bc.Content = string(data)
	}

	if bc.Truncated {
		bc.Content += "\n" + TruncatedTag
	}

	return bc
}

func (bc *BodyContent) Bytes() ([]byte, error) {
	if bc == nil {
		return nil, nil
	}
	content := bc.Content
	if bc.Truncated {
		content = strings.TrimSuffix(content, "\n"+TruncatedTag)
	}
	if bc.IsBinary {
		return base64.StdEncoding.DecodeString(content)
	}
	return []byte(content), nil
}

func isTextContent(contentType string) bool {
	ct := strings.ToLower(contentType)
	textTypes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/x-www-form-urlencoded",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"multipart/form-data",
	}
	for _, t := range textTypes {
		if strings.Contains(ct, t) {
			return true
		}
	}
	return false
}

func NowRFC3339() string {
	return time.Now().Format(time.RFC3339Nano)
}

func ParseTimestamp(ts string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, ts)
}

func Save(s *Session, path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 session 失败: %w", err)
	}
	if err := ioutil.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入 session 文件 %s 失败: %w", path, err)
	}
	return nil
}

func Load(path string) (*Session, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 session 文件 %s 失败: %w", path, err)
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		if jsonErr, ok := err.(*json.SyntaxError); ok {
			line := countLines(data[:jsonErr.Offset])
			return nil, fmt.Errorf("解析 session 文件 %s 失败 (第 %d 行): %w", path, line, err)
		}
		return nil, fmt.Errorf("解析 session 文件 %s 失败: %w", path, err)
	}

	if s.Type != SessionType {
		return nil, fmt.Errorf("无效的 session 文件类型: %s (期望: %s)", s.Type, SessionType)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("不支持的 session 版本: %d (支持的版本: %d)", s.Version, Version)
	}

	return &s, nil
}

func countLines(data []byte) int {
	count := 1
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func HeadersToMap(headers map[string][]string) map[string]string {
	result := make(map[string]string)
	for k, v := range headers {
		result[k] = strings.Join(v, ", ")
	}
	return result
}

func MapToHeaders(headers map[string]string) map[string][]string {
	result := make(map[string][]string)
	for k, v := range headers {
		result[k] = []string{v}
	}
	return result
}
