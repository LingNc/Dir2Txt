package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
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
	// 获取目标目录，默认为当前目录
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	// 获取绝对路径以确定根目录名称
	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Printf("无法获取绝对路径: %v\n", err)
		return
	}
	rootName := filepath.Base(absRoot)

	// 动态设置输出文件名: 目录名_context.md
	config.OutputFile = fmt.Sprintf("%s_context.md", rootName)

	// 创建输出文件
	outFile, err := os.Create(config.OutputFile)
	if err != nil {
		fmt.Printf("无法创建输出文件: %v\n", err)
		return
	}
	defer outFile.Close()

	// 使用 bufio 提高写入性能
	writer := bufio.NewWriter(outFile)
	defer writer.Flush()

	fmt.Printf("正在扫描目录: %s\n", root)
	fmt.Printf("结果将写入: %s\n", config.OutputFile)

	// 1. 写入项目结构树
	writer.WriteString("# Project Structure\n\n")
	writer.WriteString("```text\n")

	// 显示根目录名
	writer.WriteString(rootName + "/\n")

	if err := writeTree(root, "", writer); err != nil {
		writer.WriteString(fmt.Sprintf("Error generating tree: %v\n", err))
	}
	writer.WriteString("```\n\n")
	writer.WriteString("---\n\n")

	// 2. 遍历文件并写入内容
	writer.WriteString("# File Contents\n\n")

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := d.Name()

		// 排除输出文件自身
		if name == config.OutputFile {
			return nil
		}

		// 检查是否是"垃圾"目录或文件 (完全忽略，既不出现在树里也不处理内容)
		if isJunk(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// 如果是目录，继续遍历
		if d.IsDir() {
			return nil
		}

		// 检查是否是"资源"文件 (不处理内容，虽然它们会出现在上面的树中)
		// 例如: 图片, 普通的exe
		if isAsset(name) {
			return nil
		}

		// 处理单个文件 (读取内容)
		return processFile(path, writer)
	})

	if err != nil {
		fmt.Printf("遍历目录时出错: %v\n", err)
	} else {
		fmt.Println("完成！")
	}
}

// processFile 读取文件并格式化写入 Markdown
func processFile(path string, writer *bufio.Writer) error {
	// 1. 获取文件信息与大小检查
	info, err := os.Stat(path)
	if err != nil {
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

// writeTree 生成简单的 ASCII 目录树
func writeTree(path string, prefix string, w *bufio.Writer) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	// 过滤掉忽略的项
	var visibleEntries []os.DirEntry
	for _, entry := range entries {
		name := entry.Name()

		// 排除输出文件自身
		if name == config.OutputFile {
			continue
		}

		// 关键点：只排除"垃圾"文件 (isJunk)，不排除"资源"文件 (isAsset)
		// 这样图片和exe文件会出现在树中
		if !isJunk(name) {
			visibleEntries = append(visibleEntries, entry)
		}
	}

	for i, entry := range visibleEntries {
		isLast := i == len(visibleEntries)-1

		marker := "├── "
		if isLast {
			marker = "└── "
		}

		w.WriteString(prefix + marker + entry.Name() + "\n")

		if entry.IsDir() {
			newPrefix := prefix + "│   "
			if isLast {
				newPrefix = prefix + "    "
			}
			writeTree(filepath.Join(path, entry.Name()), newPrefix, w)
		}
	}
	return nil
}
