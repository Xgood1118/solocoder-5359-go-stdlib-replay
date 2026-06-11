package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"example.com/replay/internal/session"
)

type VariableBind struct {
	Name   string
	Source string
	Path   string
}

type VariableStore struct {
	store      map[string]string
	lastResp   *session.ResponseRecord
	lastRespBody map[string]interface{}
}

func NewVariableStore() *VariableStore {
	return &VariableStore{
		store:      make(map[string]string),
		lastRespBody: make(map[string]interface{}),
	}
}

func (vs *VariableStore) SetResponse(resp *session.ResponseRecord) {
	vs.lastResp = resp
	vs.lastRespBody = make(map[string]interface{})
	if resp != nil && resp.Body != nil && !resp.Body.IsBinary {
		content := resp.Body.Content
		if resp.Body.Truncated {
			content = strings.TrimSuffix(content, "\n"+session.TruncatedTag)
		}
		_ = json.Unmarshal([]byte(content), &vs.lastRespBody)
	}
}

func (vs *VariableStore) Set(name, value string) {
	vs.store[name] = value
}

func (vs *VariableStore) Get(name string) (string, bool) {
	val, ok := vs.store[name]
	return val, ok
}

func (vs *VariableStore) ApplyBind(bind VariableBind) error {
	switch bind.Source {
	case "env":
		val := os.Getenv(bind.Path)
		if val != "" {
			vs.store[bind.Name] = val
		}
	case "response":
		if vs.lastResp == nil {
			return fmt.Errorf("无可用的前一个响应来提取变量 %s", bind.Name)
		}
		val, err := vs.extractFromResponse(bind.Path)
		if err != nil {
			return err
		}
		vs.store[bind.Name] = val
	case "regex":
		if vs.lastResp == nil || vs.lastResp.Body == nil {
			return fmt.Errorf("无可用的前一个响应体来提取变量 %s", bind.Name)
		}
		content := vs.lastResp.Body.Content
		if vs.lastResp.Body.Truncated {
			content = strings.TrimSuffix(content, "\n"+session.TruncatedTag)
		}
		val, err := vs.extractByRegex(content, bind.Path)
		if err != nil {
			return err
		}
		vs.store[bind.Name] = val
	default:
		return fmt.Errorf("不支持的变量源: %s", bind.Source)
	}
	return nil
}

func (vs *VariableStore) extractFromResponse(path string) (string, error) {
	if strings.HasPrefix(path, "header.") {
		headerName := strings.TrimPrefix(path, "header.")
		if vs.lastResp.Headers == nil {
			return "", fmt.Errorf("响应头为空")
		}
		val, ok := vs.lastResp.Headers[headerName]
		if !ok {
			for k, v := range vs.lastResp.Headers {
				if strings.EqualFold(k, headerName) {
					return v, nil
				}
			}
			return "", fmt.Errorf("响应头中未找到 %s", headerName)
		}
		return val, nil
	}
	return vs.extractFromJSON(path)
}

func (vs *VariableStore) extractFromJSON(path string) (string, error) {
	parts := strings.Split(path, ".")
	var current interface{} = vs.lastRespBody
	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			val, ok := v[part]
			if !ok {
				return "", fmt.Errorf("JSON 路径中未找到字段 %s", part)
			}
			current = val
		default:
			return "", fmt.Errorf("JSON 路径 %s 在非对象类型上无法继续", part)
		}
	}
	switch v := current.(type) {
	case string:
		return v, nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

func (vs *VariableStore) extractByRegex(content, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("正则表达式编译失败: %w", err)
	}
	matches := re.FindStringSubmatch(content)
	if len(matches) < 2 {
		return "", fmt.Errorf("正则表达式未匹配到捕获组")
	}
	return matches[1], nil
}

var placeholderRegex = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

func (vs *VariableStore) ReplaceInString(s string) string {
	return placeholderRegex.ReplaceAllStringFunc(s, func(match string) string {
		sub := placeholderRegex.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		val, ok := vs.store[sub[1]]
		if !ok {
			return match
		}
		return val
	})
}

func (vs *VariableStore) ReplaceInRequest(req *session.RequestRecord) *session.RequestRecord {
	result := &session.RequestRecord{
		URL:       vs.ReplaceInString(req.URL),
		Method:    req.Method,
		Headers:   make(map[string]string),
		Query:     make(map[string]string),
		Timestamp: req.Timestamp,
	}
	for k, v := range req.Headers {
		result.Headers[k] = vs.ReplaceInString(v)
	}
	for k, v := range req.Query {
		result.Query[k] = vs.ReplaceInString(v)
	}
	if req.Body != nil {
		result.Body = &session.BodyContent{
			IsBinary: req.Body.IsBinary,
			Content:  vs.ReplaceInString(req.Body.Content),
			Truncated: req.Body.Truncated,
		}
	}
	return result
}

func ParseBindSpec(spec string) (VariableBind, error) {
	parts := strings.SplitN(spec, "=", 2)
	if len(parts) != 2 {
		return VariableBind{}, fmt.Errorf("无效的变量绑定格式: %s (期望 name=source:path)", spec)
	}
	name := parts[0]
	sourcePath := strings.SplitN(parts[1], ":", 2)
	if len(sourcePath) != 2 {
		return VariableBind{Name: name, Source: "env", Path: parts[1]}, nil
	}
	return VariableBind{Name: name, Source: sourcePath[0], Path: sourcePath[1]}, nil
}
