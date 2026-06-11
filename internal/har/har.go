package har

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"strings"

	"example.com/replay/internal/session"
)

type HARHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HARQuery struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HARPostData struct {
	MimeType string            `json:"mimeType"`
	Params   []HARQuery        `json:"params"`
	Text     string            `json:"text"`
}

type HARRequest struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []HARHeader `json:"headers"`
	QueryString []HARQuery  `json:"queryString"`
	PostData    *HARPostData `json:"postData,omitempty"`
	BodySize    int64       `json:"bodySize"`
}

type HARContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Encoding string `json:"encoding,omitempty"`
}

type HARResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []HARHeader `json:"headers"`
	Content     HARContent   `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	BodySize    int64       `json:"bodySize"`
}

type HAREntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         HARRequest  `json:"request"`
	Response        HARResponse `json:"response"`
}

type HARLog struct {
	Version string      `json:"version"`
	Entries []HAREntry  `json:"entries"`
}

type HARFile struct {
	Log HARLog `json:"log"`
}

func Parse(path string, maxBodySize int64) (*session.Session, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 HAR 文件 %s 失败: %w", path, err)
	}

	var har HARFile
	if err := json.Unmarshal(data, &har); err != nil {
		if jsonErr, ok := err.(*json.SyntaxError); ok {
			line := countLines(data[:jsonErr.Offset])
			return nil, fmt.Errorf("解析 HAR 文件 %s 失败 (第 %d 行): %w", path, line, err)
		}
		return nil, fmt.Errorf("解析 HAR 文件 %s 失败: %w", path, err)
	}

	if har.Log.Version != "1.2" && har.Log.Version != "1.1" {
		return nil, fmt.Errorf("不支持的 HAR 版本: %s (支持: 1.1, 1.2)", har.Log.Version)
	}

	s := session.NewSession()
	for i, entry := range har.Log.Entries {
		converted, err := convertEntry(&entry, maxBodySize, i)
		if err != nil {
			return nil, fmt.Errorf("转换第 %d 条记录失败: %w", i+1, err)
		}
		if converted != nil {
			s.Requests = append(s.Requests, converted)
		}
	}

	return s, nil
}

func convertEntry(entry *HAREntry, maxBodySize int64, idx int) (*session.Entry, error) {
	req, err := convertRequest(&entry.Request, entry.StartedDateTime, maxBodySize)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, nil
	}

	resp, err := convertResponse(&entry.Response, entry.StartedDateTime, maxBodySize)
	if err != nil {
		return nil, err
	}

	return &session.Entry{
		Request:  req,
		Response: resp,
	}, nil
}

func convertRequest(hr *HARRequest, startedDateTime string, maxBodySize int64) (*session.RequestRecord, error) {
	if hr.Method == "" || hr.URL == "" {
		return nil, nil
	}

	req := &session.RequestRecord{
		Method:    hr.Method,
		URL:       hr.URL,
		Headers:   headersToMap(hr.Headers),
		Query:     queryToMap(hr.QueryString, hr.URL),
		Timestamp: normalizeTimestamp(startedDateTime),
	}

	if hr.PostData != nil {
		bodyBytes := getPostDataBytes(hr.PostData)
		mimeType := hr.PostData.MimeType
		if mimeType == "" {
			mimeType = req.Headers["Content-Type"]
		}
		req.Body = session.NewBodyContent(bodyBytes, mimeType, maxBodySize)
	}

	return req, nil
}

func convertResponse(hr *HARResponse, startedDateTime string, maxBodySize int64) (*session.ResponseRecord, error) {
	resp := &session.ResponseRecord{
		Status:    hr.Status,
		Headers:   headersToMap(hr.Headers),
		Timestamp: normalizeTimestamp(startedDateTime),
	}

	if hr.Content.Text != "" {
		var bodyBytes []byte
		if hr.Content.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(hr.Content.Text)
			if err != nil {
				bodyBytes = []byte(hr.Content.Text)
			} else {
				bodyBytes = decoded
			}
		} else {
			bodyBytes = []byte(hr.Content.Text)
		}
		mimeType := hr.Content.MimeType
		if mimeType == "" {
			mimeType = resp.Headers["Content-Type"]
		}
		resp.Body = session.NewBodyContent(bodyBytes, mimeType, maxBodySize)
	}

	return resp, nil
}

func headersToMap(headers []HARHeader) map[string]string {
	result := make(map[string]string)
	for _, h := range headers {
		if existing, ok := result[h.Name]; ok {
			result[h.Name] = existing + ", " + h.Value
		} else {
			result[h.Name] = h.Value
		}
	}
	return result
}

func queryToMap(qs []HARQuery, rawURL string) map[string]string {
	result := make(map[string]string)
	for _, q := range qs {
		result[q.Name] = q.Value
	}
	if len(result) == 0 && strings.Contains(rawURL, "?") {
		u, err := url.Parse(rawURL)
		if err == nil {
			for k, v := range u.Query() {
				if len(v) > 0 {
					result[k] = v[0]
				}
			}
		}
	}
	return result
}

func getPostDataBytes(pd *HARPostData) []byte {
	if pd == nil {
		return nil
	}
	if pd.Text != "" {
		return []byte(pd.Text)
	}
	if len(pd.Params) > 0 {
		values := url.Values{}
		for _, p := range pd.Params {
			values.Set(p.Name, p.Value)
		}
		return []byte(values.Encode())
	}
	return nil
}

func normalizeTimestamp(ts string) string {
	if ts == "" {
		return session.NowRFC3339()
	}
	normalized := strings.ReplaceAll(ts, "/", "-")
	if strings.HasSuffix(normalized, "Z") {
		if t, err := session.ParseTimestamp(normalized); err == nil {
			return t.Format("2006-01-02T15:04:05.000Z07:00")
		}
	}
	if t, err := session.ParseTimestamp(ts); err == nil {
		return t.Format("2006-01-02T15:04:05.000Z07:00")
	}
	return session.NowRFC3339()
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
