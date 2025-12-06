package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	version         = "v1.5"
	maxDisplayFiles = 24
	keepHeadFiles   = 8
	keepTailFiles   = 8
)

// Config 配置需要忽略的目录和文件后缀
type Config struct {
	OutputFile   string
	IgnoredDirs  map[string]bool
	IgnoredExts  map[string]bool
	IgnoredFiles map[string]bool // 指定要完全隐藏的文件 (既不在树中显示，也不读取内容)
	MaxFileSize  int64           // 忽略过大的文件
	TextExts     map[string]bool // 强制视为文本的文件后缀
}

// walkFollowSymlinks 遍历目录，跟随符号链接的目录，保持逻辑路径用于过滤
func walkFollowSymlinks(root string, fn func(logicalRel string, fullPath string, d os.DirEntry) error) error {
	type node struct {
		fsPath string // 实际文件系统路径（可能为解析后的目标路径）
		rel    string // 相对 root 的逻辑路径（使用符号链接名字串接）
	}

	stack := []node{{fsPath: root, rel: ""}}
	seen := map[string]bool{}

	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(n.fsPath)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			name := entry.Name()
			logicalRel := name
			if n.rel != "" {
				logicalRel = filepath.Join(n.rel, name)
			}

			childFSPath := filepath.Join(n.fsPath, name)
			childIsDir := entry.IsDir()

			// 跟随符号链接目录
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(childFSPath)
				if err == nil {
					if info, err := os.Stat(target); err == nil && info.IsDir() {
						childIsDir = true
						childFSPath = target
					}
				}
			}

			// 先把当前条目交给回调
			if err := fn(logicalRel, childFSPath, entry); err != nil {
				if errors.Is(err, filepath.SkipDir) {
					continue
				}
				return err
			}

			if childIsDir {
				real, err := filepath.EvalSymlinks(childFSPath)
				if err == nil {
					if seen[real] {
						continue
					}
					seen[real] = true
				}
				stack = append(stack, node{fsPath: childFSPath, rel: logicalRel})
			}
		}
	}

	return nil
}

// multiValue 允许通过空格或多次传参传入多个值，例如：
// --filter "*.png *.jpg" --filter "!keep.txt"
type multiValue []string

func (m *multiValue) String() string {
	return strings.Join(*m, ",")
}

func (m *multiValue) Set(s string) error {
	if s == "" {
		return nil
	}
	parts := strings.Fields(s)
	*m = append(*m, parts...)
	return nil
}

// SimpleDirEntry 用于在目录树中创建伪造节点（如省略号）
type SimpleDirEntry struct {
	name  string
	isDir bool
}

func (e *SimpleDirEntry) Name() string               { return e.name }
func (e *SimpleDirEntry) IsDir() bool                { return e.isDir }
func (e *SimpleDirEntry) Type() os.FileMode          { return 0 }
func (e *SimpleDirEntry) Info() (os.FileInfo, error) { return nil, nil }

func parseCommandLine() (multiValue, multiValue, multiValue, string, bool, error) {
	var dirs multiValue
	var softFilters multiValue // -f / --filter / -filter : 只过滤内容，不排除树
	var hardFilters multiValue // -F / --Filter : 完全过滤，树和内容都不出现
	var out string
	var help bool
	args := os.Args[1:]
	var leftover []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" && i+1 < len(args):
			leftover = append(leftover, args[i+1:]...)
			i = len(args)
		case arg == "--dir" || arg == "-d":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, fmt.Errorf("--dir 需要一个路径")
			}
			i++
			dirs.Set(args[i])
		case strings.HasPrefix(arg, "--dir="):
			dirs.Set(strings.TrimPrefix(arg, "--dir="))
		case arg == "--filter" || arg == "-filter" || arg == "-f":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, fmt.Errorf("--filter 需要一个表达式")
			}
			i++
			softFilters.Set(args[i])
		case strings.HasPrefix(arg, "--filter=") || strings.HasPrefix(arg, "-filter="):
			softFilters.Set(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "filter="))
		case arg == "--Filter" || arg == "-Filter" || arg == "-F":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, fmt.Errorf("--Filter 需要一个表达式")
			}
			i++
			hardFilters.Set(args[i])
		case strings.HasPrefix(arg, "--Filter=") || strings.HasPrefix(arg, "-Filter="):
			hardFilters.Set(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "Filter="))
		case arg == "--out" || arg == "-o":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, fmt.Errorf("--out 需要一个路径")
			}
			i++
			out = args[i]
		case strings.HasPrefix(arg, "--out="):
			out = strings.TrimPrefix(arg, "--out=")
		case arg == "--help" || arg == "-h":
			help = true
		default:
			leftover = append(leftover, arg)
		}
	}
	for _, arg := range leftover {
		if strings.HasPrefix(arg, "!") || strings.ContainsAny(arg, "*?[]") {
			softFilters.Set(arg)
			continue
		}
		dirs.Set(arg)
	}
	return dirs, softFilters, hardFilters, out, help, nil
}

// determineOutputPath 计算最终的输出文件路径
func determineOutputPath(dirs []string, userOut string) (string, error) {
	if len(dirs) == 0 {
		return "", fmt.Errorf("至少需要一个目录")
	}
	var absDirs []string
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		absDirs = append(absDirs, filepath.Clean(abs))
	}

	fileName := buildOutputFileName(absDirs)
	if userOut == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, fileName), nil
	}

	cleanOut := filepath.Clean(userOut)
	dirHint := strings.HasSuffix(userOut, string(os.PathSeparator)) || strings.HasSuffix(userOut, "/") || strings.HasSuffix(userOut, "\\")
	if strings.EqualFold(filepath.Ext(cleanOut), ".md") {
		return cleanOut, nil
	}

	info, err := os.Stat(cleanOut)
	if err == nil && info.IsDir() {
		return filepath.Join(cleanOut, fileName), nil
	}

	// 如果 userOut 以路径分隔符结尾，也当作目录
	if dirHint {
		return filepath.Join(cleanOut, fileName), nil
	}

	// 默认按目录处理，无视是否存在
	return filepath.Join(cleanOut, fileName), nil
}

func buildOutputFileName(absDirs []string) string {
	if len(absDirs) == 1 {
		return fmt.Sprintf("%s_context.md", filepath.Base(absDirs[0]))
	}
	common := findCommonAncestor(absDirs)
	base := "merged_project"
	if common != "" && common != filepath.Dir(common) {
		base = filepath.Base(common)
	}
	if base == "" {
		base = "merged_project"
	}
	return fmt.Sprintf("%s_context.md", base)
}

func findCommonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	common := paths[0]
	for _, p := range paths[1:] {
		for !hasPathPrefix(p, common) {
			parent := filepath.Dir(common)
			if parent == common {
				return ""
			}
			common = parent
		}
	}
	return common
}

func hasPathPrefix(pathStr, prefix string) bool {
	rel, err := filepath.Rel(prefix, pathStr)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// 初始化默认配置
var config = Config{
	OutputFile: "project_context.md",
	IgnoredDirs: map[string]bool{
		".git":         true,
		".idea":        true,
		".vscode":      true,
		"node_modules": true,
		"__pycache__":  true,
		"dist":         true,
		"build":        true,
		"vendor":       true,
		"bin":          true,
		"obj":          true,
		"target":       true,
		".next":        true,
		"coverage":     true,
	},
	IgnoredExts: map[string]bool{
		// 图片/媒体
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".svg": true,
		".mp4": true, ".mp3": true, ".wav": true, ".webp": true,
		// 压缩包
		".zip": true, ".tar": true, ".gz": true, ".7z": true, ".rar": true,
		// 编译产物/二进制
		".exe": true, ".dll": true, ".so": true, ".dylib": true, ".class": true, ".pyc": true, ".o": true,
		// 字体
		".ttf": true, ".woff": true, ".woff2": true, ".eot": true,
		// 其他
		".lock": true, ".pdf": true, ".ds_store": true,
	},
	// 这些文件将被完全忽略（视为垃圾文件，不出现在目录树中）
	IgnoredFiles: map[string]bool{
		"dir2txt.go":  true,
		"dir2txt":     true,
		"dir2txt.exe": true,
	},
	TextExts: map[string]bool{
		".md": true, ".txt": true, ".log": true,
		".go": true, ".java": true, ".py": true, ".js": true, ".ts": true,
		".c": true, ".cpp": true, ".h": true, ".hpp": true,
		".html": true, ".css": true, ".xml": true, ".yaml": true, ".yml": true,
		".json": true, ".sql": true, ".properties": true, ".ini": true,
		".sh": true, ".bat": true, ".conf": true, ".toml": true,
	},
	MaxFileSize: 1024 * 1024, // 1MB
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "dir2txt %s\n", version)
		fmt.Fprintf(flag.CommandLine.Output(), "用法: dir2txt [--dir <path> ...] [--filter <pattern> ...] [dir|filter ...]\n")
		fmt.Fprintf(flag.CommandLine.Output(), "示例:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  dir2txt --dir . ../other --filter '*.png *.jpg' '!keep.png'\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  dir2txt --filter '*.png' --filter '!keep.png' src test\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  dir2txt -F 'dist/**' -f '*.png' src\n")
		fmt.Fprintf(flag.CommandLine.Output(), "参数:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --dir/-d      指定要扫描的目录，可重复；也可用位置参数追加目录\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --filter/-f   软过滤：仅跳过文件内容输出，目录和树仍显示；支持 * ? [] 与 ! 反向\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --Filter/-F   硬过滤：目录树和文件内容都不显示；支持 * ? [] 与 ! 反向\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --out/-o      指定输出文件路径或输出目录\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  位置参数      未被 --dir 消耗的参数：若含 * ? [] 或以 ! 开头视为软过滤，其它视为目录\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --help/-h     显示此帮助\n")
	}

	parsedDirs, parsedSoftFilters, parsedHardFilters, outFlag, help, err := parseCommandLine()
	if help {
		flag.Usage()
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		flag.Usage()
		os.Exit(1)
	}

	dirs := []string(parsedDirs)
	softFilters := []string(parsedSoftFilters)
	hardFilters := []string(parsedHardFilters)
	if len(dirs) == 0 {
		dirs = append(dirs, ".")
	}

	finalOutPath, err := determineOutputPath(dirs, outFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 无法确定输出路径: %v\n", err)
		os.Exit(1)
	}

	config.OutputFile = filepath.Base(finalOutPath)
	if err := os.MkdirAll(filepath.Dir(finalOutPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建输出目录: %v\n", err)
		os.Exit(1)
	}

	outFile, err := os.Create(finalOutPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法创建输出文件: %v\n", err)
		os.Exit(1)
	}
	defer outFile.Close()

	writer := bufio.NewWriter(outFile)
	defer writer.Flush()

	fmt.Printf("结果将写入: %s\n", finalOutPath)

	if err := processDirs(dirs, softFilters, hardFilters, writer, finalOutPath); err != nil {
		fmt.Fprintf(os.Stderr, "处理目录失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("完成！")
}

func processDirs(dirs []string, softFilters []string, hardFilters []string, writer *bufio.Writer, finalOutPath string) error {
	absOut, err := filepath.Abs(finalOutPath)
	if err != nil {
		return err
	}

	writer.WriteString("# Project Structure\n\n")
	writer.WriteString("```text\n")
	for _, dir := range dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			writer.WriteString(fmt.Sprintf("%s/\n", dir))
			writer.WriteString(fmt.Sprintf("Error generating tree: %v\n", err))
			continue
		}
		writer.WriteString(filepath.Base(absDir) + "/\n")
		if err := writeTree(absDir, absDir, absDir, absDir, "", writer, hardFilters, map[string]bool{}); err != nil {
			writer.WriteString(fmt.Sprintf("Error generating tree for %s: %v\n", dir, err))
		}
		writer.WriteString("\n")
	}
	writer.WriteString("```\n\n")
	writer.WriteString("---\n\n")

	writer.WriteString("# File Contents\n\n")
	var firstErr error
	for _, dir := range dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法获取目录 %s 绝对路径: %v\n", dir, err)
			firstErr = err
			continue
		}
		err = walkFollowSymlinks(absDir, func(logicalRel string, fullPath string, d os.DirEntry) error {
			// 排除输出文件自身
			absPath := fullPath
			if absPath == absOut {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			name := d.Name()
			if isJunk(name) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			relSlash := filepath.ToSlash(logicalRel)
			if relSlash == "." {
				relSlash = ""
			}
			if relSlash != "" && isFiltered(relSlash, hardFilters) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				return nil
			}

			if isAsset(name) || isFiltered(relSlash, softFilters) || isFiltered(name, softFilters) {
				return nil
			}

			return processFile(fullPath, writer)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "处理目录 %s 时出错: %v\n", dir, err)
			firstErr = err
		}
	}
	return firstErr
}

// processFile 读取文件并格式化写入 Markdown
func processFile(path string, writer *bufio.Writer) error {
	// 1. 获取文件信息与大小检查
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	// 软链接指向目录时跳过内容读取
	if info.IsDir() {
		fmt.Printf("[SKIP] 软链接指向目录: %s\n", path)
		return nil
	}
	if info.Size() > config.MaxFileSize {
		fmt.Printf("[SKIP] 大文件 (>1MB): %s\n", path)
		return nil
	}

	// 2. 读取文件内容
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	isForceText := config.TextExts[ext]

	// 3. 二进制检查（非白名单才检查）
	if !isForceText && isBinary(content) {
		fmt.Printf("[SKIP] 检测到二进制文件: %s\n", path)
		return nil
	}

	// 4. 编码检测与转换
	utf8Content, encoding, err := convertToUTF8(content)
	if err != nil {
		fmt.Printf("[WARN] 无法识别文件编码 (已跳过): %s\n", path)
		fmt.Printf("       -> 原因: 内容非 UTF-8 且非 GBK，或包含非法字符。\n")
		return nil
	}

	// 5. 如果发生了转码，发出通知
	if encoding != "UTF-8" {
		fmt.Printf("[INFO] 自动转换编码 [%s -> UTF-8]: %s\n", encoding, path)
	}

	// 6. 写入 Markdown
	fmt.Printf("正在处理: %s\n", path)

	// 标准化路径分隔符
	displayPath := filepath.ToSlash(path)

	// 确定代码块语言标记
	codeBlockLang := strings.TrimPrefix(ext, ".")
	if codeBlockLang == "" {
		codeBlockLang = "text"
	}

	writer.WriteString(fmt.Sprintf("## File: %s\n\n", displayPath))
	writer.WriteString(fmt.Sprintf("```%s\n", codeBlockLang))
	writer.Write(utf8Content)

	// 确保代码块如果没换行符结尾，手动补一个
	if len(utf8Content) > 0 && utf8Content[len(utf8Content)-1] != '\n' {
		writer.WriteString("\n")
	}

	writer.WriteString("```\n\n")
	writer.WriteString("---\n\n")

	return nil
}

// isFiltered 使用类似 gitignore 的简单通配符规则判断是否应被过滤
// 规则：按传入顺序匹配，后匹配覆盖前匹配；前缀 ! 表示反向（取消过滤）
func isFiltered(relPath string, filters []string) bool {
	if relPath == "" {
		return false
	}

	// 使用统一的正斜杠
	norm := filepath.ToSlash(relPath)
	ignored := false

	for _, p := range filters {
		negate := strings.HasPrefix(p, "!")
		pat := strings.TrimPrefix(p, "!")
		if pat == "" {
			continue
		}
		matched, err := path.Match(pat, norm)
		if err != nil {
			continue
		}
		if matched {
			ignored = !negate
		}
	}

	return ignored
}

// isJunk 检查是否为"垃圾"文件/目录 (不应该出现在任何地方)
// 例如: .git, node_modules, .DS_Store, code2md.exe
func isJunk(name string) bool {
	// 关键修复：当前目录 "." 不是垃圾文件
	if name == "." {
		return false
	}

	// 特例：保留 .env 和 .gitignore，虽然它们以点开头，但通常很重要
	if name == ".env" || name == ".gitignore" {
		return false
	}

	// 1. 检查特定文件名忽略列表 (如 code2md.exe) - 这里是完全隐藏
	if config.IgnoredFiles[name] {
		return true
	}

	// 2. 忽略隐藏文件/目录 (以 . 开头)
	if strings.HasPrefix(name, ".") {
		return true
	}

	// 3. 忽略配置中指定的目录 (如 node_modules)
	if config.IgnoredDirs[name] {
		return true
	}

	return false
}

// isAsset 检查是否为"资源"文件 (应该出现在目录树中，但不读取内容)
// 例如: 图片, 普通可执行文件
func isAsset(name string) bool {
	// 检查文件扩展名 (如 .png, .exe)
	ext := strings.ToLower(filepath.Ext(name))
	if config.IgnoredExts[ext] {
		return true
	}
	return false
}

// isBinary 通过检查内容中是否包含 NUL 字节来简单判断是否为二进制文件
func isBinary(content []byte) bool {
	checkLen := 512
	if len(content) < checkLen {
		checkLen = len(content)
	}

	// 真正的二进制文件通常包含 NUL 字节
	if bytes.IndexByte(content[:checkLen], 0) != -1 {
		return true
	}

	return false
}

// convertToUTF8 尝试将内容转换为 UTF-8
// 返回: (转换后的内容, 原始编码名称, error)
func convertToUTF8(content []byte) ([]byte, string, error) {
	// 1. 先尝试 UTF-8 校验
	if utf8.Valid(content) {
		return content, "UTF-8", nil
	}

	// 2. 尝试 GBK / GB18030 解码
	reader := transform.NewReader(bytes.NewReader(content), simplifiedchinese.GBK.NewDecoder())
	decoded, err := io.ReadAll(reader)
	if err == nil {
		if utf8.Valid(decoded) {
			return decoded, "GBK/GB18030", nil
		}
	}

	// 3. 其他编码可在此扩展
	return nil, "Unknown", fmt.Errorf("encoding not recognized")
}

// writeTree 生成简单的 ASCII 目录树，支持文件折叠，跟随符号链接目录但使用逻辑路径做过滤
func writeTree(rootFS string, rootLogical string, currentFS string, currentLogical string, prefix string, w *bufio.Writer, hardFilters []string, seen map[string]bool) error {
	entries, err := os.ReadDir(currentFS)
	if err != nil {
		return err
	}

	// 过滤掉忽略的项
	var visibleEntries []os.DirEntry
	for _, entry := range entries {
		name := entry.Name()
		logicalPath := filepath.Join(currentLogical, name)
		rel, _ := filepath.Rel(rootLogical, logicalPath)
		relSlash := filepath.ToSlash(rel)

		// 排除输出文件自身
		if name == config.OutputFile {
			continue
		}

		// 关键点：只排除"垃圾"文件 (isJunk)，不排除"资源"文件 (isAsset)
		// 这样图片和exe文件会出现在树中
		if isJunk(name) {
			continue
		}

		// 过滤表达式处理（对目录树也生效，仅使用 hardFilters）
		if relSlash != "" && isFiltered(relSlash, hardFilters) {
			if entry.IsDir() {
				continue
			}
			continue
		}

		visibleEntries = append(visibleEntries, entry)
	}

	// 分离目录与文件，文件过多时折叠
	var dirs []os.DirEntry
	var files []os.DirEntry
	for _, e := range visibleEntries {
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}

	if len(files) > maxDisplayFiles {
		display := make([]os.DirEntry, 0, keepHeadFiles+keepTailFiles+1)
		display = append(display, files[:keepHeadFiles]...)
		hiddenCount := len(files) - keepHeadFiles - keepTailFiles
		if hiddenCount < 0 {
			hiddenCount = 0
		}
		display = append(display, &SimpleDirEntry{name: fmt.Sprintf("... (%d files hidden) ...", hiddenCount)})
		display = append(display, files[len(files)-keepTailFiles:]...)
		files = display
	}

	finalEntries := make([]os.DirEntry, 0, len(dirs)+len(files))
	finalEntries = append(finalEntries, dirs...)
	finalEntries = append(finalEntries, files...)

	for i, entry := range finalEntries {
		isLast := i == len(finalEntries)-1

		marker := "├── "
		if isLast {
			marker = "└── "
		}

		displayName := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			fullPath := filepath.Join(currentFS, entry.Name())
			if target, err := os.Readlink(fullPath); err == nil {
				displayName = fmt.Sprintf("%s -> %s", displayName, target)
			}
		}

		w.WriteString(prefix + marker + displayName + "\n")

		childPathFS := filepath.Join(currentFS, entry.Name())
		childPathLogical := filepath.Join(currentLogical, entry.Name())
		childIsDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			if target, err := filepath.EvalSymlinks(childPathFS); err == nil {
				if info, err := os.Stat(target); err == nil && info.IsDir() {
					childIsDir = true
					childPathFS = target
				}
			}
		}

		if childIsDir {
			real, err := filepath.EvalSymlinks(childPathFS)
			if err == nil {
				if seen[real] {
					continue
				}
				seen[real] = true
			}
			newPrefix := prefix + "│   "
			if isLast {
				newPrefix = prefix + "    "
			}
			writeTree(rootFS, rootLogical, childPathFS, childPathLogical, newPrefix, w, hardFilters, seen)
		}
	}
	return nil
}
