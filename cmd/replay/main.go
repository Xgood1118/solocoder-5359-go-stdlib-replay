package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"example.com/replay/internal/diff"
	"example.com/replay/internal/har"
	"example.com/replay/internal/proxy"
	"example.com/replay/internal/replay"
	"example.com/replay/internal/session"
)

var usageText = `replay - HTTP请求录制与回放工具

用法:
  replay <command> [options]

命令:
  record    从HAR文件导入录制数据
  capture   启动HTTP代理录制浏览器请求
  run       回放已录制的请求
  diff      对比原始响应与回放响应

使用 "replay <command> --help" 查看各命令的详细帮助。
`

var recordUsage = `用法: replay record --input <har.json> --output <session.replay> [options]

从 HAR 文件 (Chrome/Firefox/Charles 导出) 导入录制数据并转换为 replay session 格式。

选项:
  --input string        输入 HAR 文件路径 (必需)
  --output string       输出 session 文件路径 (必需)
  --max-body-size size  响应体最大大小，超过则截断 (如 1MB, 512KB)，默认不限制
`

var captureUsage = `用法: replay capture --listen <:8888> --output <session.replay> [options]

启动一个 HTTP 代理服务器，浏览器配置代理到该端口即可自动录制所有请求。

选项:
  --listen string       代理监听地址，如 :8888 (必需)
  --output string       输出 session 文件路径 (必需)
  --max-body-size size  响应体最大大小，超过则截断 (如 1MB, 512KB)，默认不限制
  --exclude list        逗号分隔的URL子串列表，匹配的请求不记录
  --include-only list   逗号分隔的URL子串白名单，仅记录匹配的请求
`

var runUsage = `用法: replay run --session <session.replay> [options]

回放 session 文件中录制的请求。

选项:
  --session string      session 文件路径 (必需)
  --output string       回放结果输出路径 (默认 ./result.json)
  --loop int            循环回放次数 (默认 1)
  --filter string       按 URL 子串过滤请求
  --method string       按 HTTP 方法过滤请求 (如 POST)
  --bind name=spec      变量绑定，如 token=response:user.token 或 key=ENV_VAR
                        支持的源:
                          env:VAR_NAME          - 从环境变量读取
                          response:field.path   - 从上一次响应体JSON路径提取
                          response:header.Name  - 从上一次响应头提取
                          regex:pattern         - 从上一次响应体用正则捕获组提取
  --rps int             每秒最大请求数 (令牌桶限速)
  --concurrency int     最大并发数 (默认 1)
  --delay duration      每个请求之间的固定等待时间 (如 100ms)
  --random-delay min,max  两个请求之间的随机等待区间 (如 100,500 单位ms)
  --offset duration     所有请求时间戳偏移量 (如 1h 往后挪1小时)
  --dry-run             不实际发送请求，仅打印将要发送的请求
`

var diffUsage = `用法: replay diff --original <session.replay> --replayed <result.json> [options]

对比原始 session 与回放结果，输出人类可读的差异报告。

选项:
  --original string     原始 session 文件路径 (必需)
  --replayed string     回放结果文件路径 (必需)
  --ignore-headers      跳过响应头对比
  --ignore-body         跳过响应体对比
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "record":
		err = cmdRecord(args)
	case "capture":
		err = cmdCapture(args)
	case "run":
		err = cmdRun(args)
	case "diff":
		err = cmdDiff(args)
	case "-h", "--help", "help":
		fmt.Print(usageText)
	default:
		fmt.Printf("未知命令: %s\n\n", cmd)
		fmt.Print(usageText)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(recordUsage) }

	input := fs.String("input", "", "输入 HAR 文件路径")
	output := fs.String("output", "", "输出 session 文件路径")
	maxBodySizeStr := fs.String("max-body-size", "", "响应体最大大小")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *input == "" || *output == "" {
		fmt.Print(recordUsage)
		return fmt.Errorf("--input 和 --output 都是必需参数")
	}

	maxBodySize, err := parseSize(*maxBodySizeStr)
	if err != nil {
		return fmt.Errorf("解析 --max-body-size 失败: %w", err)
	}

	s, err := har.Parse(*input, maxBodySize)
	if err != nil {
		return err
	}

	if err := session.Save(s, *output); err != nil {
		return err
	}

	fmt.Printf("成功: 已将 %d 条请求从 HAR 文件导入到 %s\n", len(s.Requests), *output)
	return nil
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(captureUsage) }

	listen := fs.String("listen", "", "代理监听地址")
	output := fs.String("output", "", "输出 session 文件路径")
	maxBodySizeStr := fs.String("max-body-size", "", "响应体最大大小")
	excludeStr := fs.String("exclude", "", "逗号分隔的URL排除列表")
	includeOnlyStr := fs.String("include-only", "", "逗号分隔的URL白名单")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *listen == "" || *output == "" {
		fmt.Print(captureUsage)
		return fmt.Errorf("--listen 和 --output 都是必需参数")
	}

	maxBodySize, err := parseSize(*maxBodySizeStr)
	if err != nil {
		return fmt.Errorf("解析 --max-body-size 失败: %w", err)
	}

	var exclude []string
	if *excludeStr != "" {
		exclude = strings.Split(*excludeStr, ",")
		for i := range exclude {
			exclude[i] = strings.TrimSpace(exclude[i])
		}
	}

	var includeOnly []string
	if *includeOnlyStr != "" {
		includeOnly = strings.Split(*includeOnlyStr, ",")
		for i := range includeOnly {
			includeOnly[i] = strings.TrimSpace(includeOnly[i])
		}
	}

	cfg := proxy.Config{
		ListenAddr:  *listen,
		OutputPath:  *output,
		MaxBodySize: maxBodySize,
		Exclude:     exclude,
		IncludeOnly: includeOnly,
	}

	p := proxy.NewRecordingProxy(cfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n正在停止录制并保存数据...")
		_ = p.Stop()
		os.Exit(0)
	}()

	return p.Start()
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(runUsage) }

	sessionPath := fs.String("session", "", "session 文件路径")
	output := fs.String("output", "./result.json", "回放结果输出路径")
	loop := fs.Int("loop", 1, "循环回放次数")
	filter := fs.String("filter", "", "URL子串过滤")
	method := fs.String("method", "", "HTTP方法过滤")
	bindStrs := multiFlag{}
	fs.Var(&bindStrs, "bind", "变量绑定")
	rps := fs.Int("rps", 0, "每秒最大请求数")
	concurrency := fs.Int("concurrency", 1, "最大并发数")
	delayStr := fs.String("delay", "", "固定等待时间 (如 100ms)")
	randomDelayStr := fs.String("random-delay", "", "随机等待区间 (如 100,500)")
	offsetStr := fs.String("offset", "", "时间戳偏移量 (如 1h)")
	dryRun := fs.Bool("dry-run", false, "仅打印不发送")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *sessionPath == "" {
		fmt.Print(runUsage)
		return fmt.Errorf("--session 是必需参数")
	}

	var delay time.Duration
	if *delayStr != "" {
		d, err := time.ParseDuration(*delayStr)
		if err != nil {
			return fmt.Errorf("解析 --delay 失败: %w", err)
		}
		delay = d
	}

	var randomDelay [2]time.Duration
	if *randomDelayStr != "" {
		parts := strings.Split(*randomDelayStr, ",")
		if len(parts) != 2 {
			return fmt.Errorf("解析 --random-delay 失败: 格式应为 min,max (如 100,500 单位ms)")
		}
		minMs, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return fmt.Errorf("解析 --random-delay 最小值失败: %w", err)
		}
		maxMs, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("解析 --random-delay 最大值失败: %w", err)
		}
		randomDelay[0] = time.Duration(minMs) * time.Millisecond
		randomDelay[1] = time.Duration(maxMs) * time.Millisecond
	}

	var offset time.Duration
	if *offsetStr != "" {
		o, err := time.ParseDuration(*offsetStr)
		if err != nil {
			return fmt.Errorf("解析 --offset 失败: %w", err)
		}
		offset = o
	}

	var binds []replay.VariableBind
	for _, bs := range bindStrs {
		b, err := replay.ParseBindSpec(bs)
		if err != nil {
			return fmt.Errorf("解析 --bind %s 失败: %w", bs, err)
		}
		binds = append(binds, b)
	}

	cfg := replay.Config{
		SessionPath: *sessionPath,
		OutputPath:  *output,
		Loop:        *loop,
		FilterURL:   *filter,
		Method:      *method,
		Binds:       binds,
		RPS:         *rps,
		Concurrency: *concurrency,
		Delay:       delay,
		RandomDelay: randomDelay,
		Offset:      offset,
		DryRun:      *dryRun,
	}

	result, err := replay.Run(cfg)
	if err != nil {
		return err
	}

	successCount := 0
	failCount := 0
	for _, e := range result.Entries {
		if e.Status == "success" {
			successCount++
		} else if e.Status == "failed" {
			failCount++
		}
	}

	fmt.Printf("\n===== 回放完成 =====\n")
	fmt.Printf("总请求数: %d\n", len(result.Entries))
	fmt.Printf("成功: %d, 失败: %d\n", successCount, failCount)
	if *output != "" {
		fmt.Printf("结果已保存到: %s\n", *output)
	}

	if failCount > 0 {
		return fmt.Errorf("有 %d 个请求失败", failCount)
	}
	return nil
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(diffUsage) }

	original := fs.String("original", "", "原始 session 文件路径")
	replayed := fs.String("replayed", "", "回放结果文件路径")
	ignoreHeaders := fs.Bool("ignore-headers", false, "跳过响应头对比")
	ignoreBody := fs.Bool("ignore-body", false, "跳过响应体对比")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *original == "" || *replayed == "" {
		fmt.Print(diffUsage)
		return fmt.Errorf("--original 和 --replayed 都是必需参数")
	}

	cfg := diff.Config{
		OriginalPath:  *original,
		ReplayedPath:  *replayed,
		IgnoreHeaders: *ignoreHeaders,
		IgnoreBody:    *ignoreBody,
	}

	report, err := diff.Compare(cfg)
	if err != nil {
		return err
	}

	fmt.Print(diff.FormatReport(report))
	return nil
}

func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.ToLower(strings.TrimSpace(s))
	units := map[string]int64{
		"b":  1,
		"kb": 1024,
		"mb": 1024 * 1024,
		"gb": 1024 * 1024 * 1024,
	}
	for suffix, multiplier := range units {
		if strings.HasSuffix(s, suffix) {
			numStr := strings.TrimSuffix(s, suffix)
			num, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
			if err != nil {
				return 0, err
			}
			return int64(num * float64(multiplier)), nil
		}
	}
	num, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的大小格式: %s (支持 B/KB/MB/GB)", s)
	}
	return num, nil
}

type multiFlag []string

func (m *multiFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}
