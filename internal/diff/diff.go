package diff

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"reflect"
	"sort"
	"strings"

	"example.com/replay/internal/session"
)

type Config struct {
	OriginalPath   string
	ReplayedPath   string
	IgnoreHeaders  bool
	IgnoreBody     bool
}

type HeaderDiff struct {
	Name        string
	Original    string
	Replayed    string
	IsChanged   bool
}

type BodyFieldDiff struct {
	Path      string
	Original  interface{}
	Replayed  interface{}
	ChangeType string
}

type EntryDiff struct {
	Index         int
	URL           string
	Method        string
	StatusChanged bool
	OriginalStatus int
	ReplayedStatus int
	Headers       []HeaderDiff
	Body          []BodyFieldDiff
	BodySkipped   bool
	SkipReason    string
}

type Report struct {
	TotalEntries   int
	ChangedEntries int
	Entries        []EntryDiff
}

func Compare(cfg Config) (*Report, error) {
	origSession, err := session.Load(cfg.OriginalPath)
	if err != nil {
		return nil, err
	}

	replayedResult, err := loadReplayedResult(cfg.ReplayedPath)
	if err != nil {
		return nil, err
	}

	report := &Report{
		TotalEntries:   len(origSession.Requests),
		Entries:        make([]EntryDiff, 0, len(origSession.Requests)),
	}

	minLen := len(origSession.Requests)
	if len(replayedResult) < minLen {
		minLen = len(replayedResult)
	}

	for i := 0; i < minLen; i++ {
		origEntry := origSession.Requests[i]
		replayedEntry := replayedResult[i]
		if origEntry == nil || replayedEntry == nil {
			continue
		}

		ed := compareEntry(i, origEntry, replayedEntry, cfg)
		if ed.StatusChanged || hasChangedHeaders(ed.Headers) || len(ed.Body) > 0 {
			report.ChangedEntries++
		}
		report.Entries = append(report.Entries, ed)
	}

	return report, nil
}

func compareEntry(idx int, orig *session.Entry, replayed *ReplayedEntry, cfg Config) EntryDiff {
	ed := EntryDiff{
		Index:          idx,
		URL:            orig.Request.URL,
		Method:         orig.Request.Method,
	}

	if orig.Response != nil {
		ed.OriginalStatus = orig.Response.Status
	}
	if replayed.Response != nil {
		ed.ReplayedStatus = replayed.Response.Status
	}
	ed.StatusChanged = ed.OriginalStatus != ed.ReplayedStatus

	if !cfg.IgnoreHeaders {
		origHeaders := map[string]string{}
		replayedHeaders := map[string]string{}
		if orig.Response != nil && orig.Response.Headers != nil {
			origHeaders = orig.Response.Headers
		}
		if replayed.Response != nil && replayed.Response.Headers != nil {
			replayedHeaders = replayed.Response.Headers
		}
		ed.Headers = compareHeaders(origHeaders, replayedHeaders)
	}

	if !cfg.IgnoreBody {
		ed.Body, ed.BodySkipped, ed.SkipReason = compareBodies(orig.Response, replayed.Response)
	}

	return ed
}

func compareHeaders(orig, replayed map[string]string) []HeaderDiff {
	result := make([]HeaderDiff, 0)
	allKeys := make(map[string]bool)
	for k := range orig {
		allKeys[k] = true
	}
	for k := range replayed {
		allKeys[k] = true
	}

	keys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		origVal := orig[k]
		replayedVal := replayed[k]
		if origVal == "" {
			for ok, ov := range orig {
				if strings.EqualFold(ok, k) {
					origVal = ov
					break
				}
			}
		}
		if replayedVal == "" {
			for rk, rv := range replayed {
				if strings.EqualFold(rk, k) {
					replayedVal = rv
					break
				}
			}
		}

		hd := HeaderDiff{
			Name:      k,
			Original:  origVal,
			Replayed:  replayedVal,
			IsChanged: origVal != replayedVal,
		}
		result = append(result, hd)
	}
	return result
}

func hasChangedHeaders(headers []HeaderDiff) bool {
	for _, h := range headers {
		if h.IsChanged {
			return true
		}
	}
	return false
}

func compareBodies(orig, replayed *session.ResponseRecord) ([]BodyFieldDiff, bool, string) {
	origCT := getContentType(orig)
	replayedCT := getContentType(replayed)

	if isHTML(origCT) && isHTML(replayedCT) {
		return nil, true, "响应均为 HTML，跳过 diff"
	}

	origBody := getBodyContent(orig)
	replayedBody := getBodyContent(replayed)

	if origBody == "" && replayedBody == "" {
		return nil, true, "两个响应体均为空"
	}

	if origBody != replayedBody {
		if isJSON(origCT) && isJSON(replayedCT) {
			return diffJSONBodies(origBody, replayedBody), false, ""
		}
		return []BodyFieldDiff{{
			Path:       "(body)",
			Original:   truncate(origBody, 200),
			Replayed:   truncate(replayedBody, 200),
			ChangeType: "value_changed",
		}}, false, ""
	}

	return nil, false, ""
}

func getBodyContent(resp *session.ResponseRecord) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	content := resp.Body.Content
	if resp.Body.Truncated {
		content = strings.TrimSuffix(content, "\n"+session.TruncatedTag)
	}
	if resp.Body.IsBinary {
		return "(binary data, " + fmt.Sprintf("%d bytes base64", len(content)) + ")"
	}
	return content
}

func diffJSONBodies(origContent, replayedContent string) []BodyFieldDiff {
	var origJSON, replayedJSON interface{}
	_ = json.Unmarshal([]byte(origContent), &origJSON)
	_ = json.Unmarshal([]byte(replayedContent), &replayedJSON)
	return compareJSONValues("", origJSON, replayedJSON)
}

func compareJSONValues(prefix string, orig, replayed interface{}) []BodyFieldDiff {
	diffs := make([]BodyFieldDiff, 0)

	if orig == nil && replayed == nil {
		return diffs
	}

	switch o := orig.(type) {
	case map[string]interface{}:
		r, ok := replayed.(map[string]interface{})
		if !ok {
			diffs = append(diffs, BodyFieldDiff{
				Path:       prefix,
				Original:   orig,
				Replayed:   replayed,
				ChangeType: "type_changed",
			})
			return diffs
		}
		return compareJSONObjects(prefix, o, r)
	case []interface{}:
		r, ok := replayed.([]interface{})
		if !ok {
			diffs = append(diffs, BodyFieldDiff{
				Path:       prefix,
				Original:   orig,
				Replayed:   replayed,
				ChangeType: "type_changed",
			})
			return diffs
		}
		return compareJSONArrays(prefix, o, r)
	default:
		if !reflect.DeepEqual(orig, replayed) {
			diffs = append(diffs, BodyFieldDiff{
				Path:       prefix,
				Original:   orig,
				Replayed:   replayed,
				ChangeType: "value_changed",
			})
		}
		return diffs
	}
}

func compareJSONObjects(prefix string, orig, replayed map[string]interface{}) []BodyFieldDiff {
	diffs := make([]BodyFieldDiff, 0)
	allKeys := make(map[string]bool)
	for k := range orig {
		allKeys[k] = true
	}
	for k := range replayed {
		allKeys[k] = true
	}

	keys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		origVal, origOk := orig[k]
		replayedVal, replayedOk := replayed[k]

		if origOk && !replayedOk {
			diffs = append(diffs, BodyFieldDiff{
				Path:       path,
				Original:   origVal,
				Replayed:   nil,
				ChangeType: "deleted",
			})
			continue
		}
		if !origOk && replayedOk {
			diffs = append(diffs, BodyFieldDiff{
				Path:       path,
				Original:   nil,
				Replayed:   replayedVal,
				ChangeType: "added",
			})
			continue
		}
		diffs = append(diffs, compareJSONValues(path, origVal, replayedVal)...)
	}
	return diffs
}

func compareJSONArrays(prefix string, orig, replayed []interface{}) []BodyFieldDiff {
	diffs := make([]BodyFieldDiff, 0)
	minLen := len(orig)
	if len(replayed) < minLen {
		minLen = len(replayed)
	}

	for i := 0; i < minLen; i++ {
		path := fmt.Sprintf("%s[%d]", prefix, i)
		diffs = append(diffs, compareJSONValues(path, orig[i], replayed[i])...)
	}

	if len(orig) > len(replayed) {
		for i := len(replayed); i < len(orig); i++ {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			diffs = append(diffs, BodyFieldDiff{
				Path:       path,
				Original:   orig[i],
				Replayed:   nil,
				ChangeType: "deleted",
			})
		}
	}
	if len(replayed) > len(orig) {
		for i := len(orig); i < len(replayed); i++ {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			diffs = append(diffs, BodyFieldDiff{
				Path:       path,
				Original:   nil,
				Replayed:   replayed[i],
				ChangeType: "added",
			})
		}
	}
	return diffs
}

func getContentType(resp *session.ResponseRecord) string {
	if resp == nil || resp.Headers == nil {
		return ""
	}
	ct := resp.Headers["Content-Type"]
	if ct == "" {
		ct = resp.Headers["content-type"]
	}
	return ct
}

func isHTML(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/html")
}

func isJSON(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "application/json")
}

func FormatReport(report *Report) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("===== 响应对比报告 =====\n"))
	sb.WriteString(fmt.Sprintf("总记录数: %d\n", report.TotalEntries))
	sb.WriteString(fmt.Sprintf("有变化记录数: %d\n\n", report.ChangedEntries))

	for _, entry := range report.Entries {
		if !entry.StatusChanged && !hasChangedHeaders(entry.Headers) && len(entry.Body) == 0 && !entry.BodySkipped {
			continue
		}

		sb.WriteString(fmt.Sprintf("--- 记录 #%d %s %s ---\n", entry.Index+1, entry.Method, entry.URL))

		if entry.StatusChanged {
			sb.WriteString(fmt.Sprintf("  [状态码变化] %d -> %d\n", entry.OriginalStatus, entry.ReplayedStatus))
		}

		if len(entry.Headers) > 0 {
			changedHeaders := make([]HeaderDiff, 0)
			for _, h := range entry.Headers {
				if h.IsChanged {
					changedHeaders = append(changedHeaders, h)
				}
			}
			if len(changedHeaders) > 0 {
				sb.WriteString("  [响应头变化]\n")
				sb.WriteString(fmt.Sprintf("  %-30s %-30s %-30s %s\n", "Header", "Original", "Replayed", "Changed"))
				sb.WriteString(fmt.Sprintf("  %s\n", strings.Repeat("-", 100)))
				for _, h := range changedHeaders {
					changedFlag := "YES"
					sb.WriteString(fmt.Sprintf("  %-30s %-30s %-30s %s\n",
						truncate(h.Name, 28),
						truncate(h.Original, 28),
						truncate(h.Replayed, 28),
						changedFlag))
				}
			}
		}

		if entry.BodySkipped {
			sb.WriteString(fmt.Sprintf("  [响应体跳过] %s\n", entry.SkipReason))
		} else if len(entry.Body) > 0 {
			sb.WriteString("  [响应体字段变化]\n")
			for _, b := range entry.Body {
				origStr := fmt.Sprintf("%v", b.Original)
				replayedStr := fmt.Sprintf("%v", b.Replayed)
				sb.WriteString(fmt.Sprintf("    - [%s] %s: %s -> %s\n",
					b.ChangeType, b.Path, truncate(origStr, 40), truncate(replayedStr, 40)))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

type ReplayedEntry struct {
	Request  *session.RequestRecord  `json:"replayed_request"`
	Response *session.ResponseRecord `json:"replayed_response"`
	Status   string                  `json:"status"`
}

func loadReplayedResult(path string) ([]*ReplayedEntry, error) {
	var result struct {
		Entries []*ReplayedEntry `json:"entries"`
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("解析回放结果文件 %s 失败: %w", path, err)
	}
	return result.Entries, nil
}

func readFile(path string) ([]byte, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件 %s 失败: %w", path, err)
	}
	return data, nil
}
