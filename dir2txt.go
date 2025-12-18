package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	version         = "v1.7.0"
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
	NoFold       bool            // 是否关闭目录树文件折叠
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

// rawStringList 用于存储目录路径，不做空格拆分
// 解决含空格目录名被拆成多个参数的问题
type rawStringList []string

func (m *rawStringList) String() string {
	return strings.Join(*m, ",")
}

func (m *rawStringList) Set(s string) error {
	if s == "" {
		return nil
	}
	*m = append(*m, s)
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

func parseCommandLine() (rawStringList, multiValue, multiValue, string, bool, bool, bool, error) {
	var dirs rawStringList
	var softFilters multiValue // -f / --filter / -filter : 只过滤内容，不排除树
	var hardFilters multiValue // -F / --Filter : 完全过滤，树和内容都不出现
	var out string
	var help bool
	var install bool
	var uninstall bool
	args := os.Args[1:]
	var leftover []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" && i+1 < len(args):
			leftover = append(leftover, args[i+1:]...)
			i = len(args)
		case arg == "--install":
			install = true
		case arg == "--uninstall":
			uninstall = true
		case arg == "--no-fold":
			config.NoFold = true
		case arg == "--config" || arg == "-c" || arg == "-fc":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--config 需要一个文件路径")
			}
			i++
			patterns, err := loadPatternsFromFile(args[i])
			if err != nil {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, err
			}
			for _, p := range patterns {
				softFilters = append(softFilters, p)
			}
		case strings.HasPrefix(arg, "--config="):
			patterns, err := loadPatternsFromFile(strings.TrimPrefix(arg, "--config="))
			if err != nil {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, err
			}
			for _, p := range patterns {
				softFilters = append(softFilters, p)
			}
		case strings.HasPrefix(arg, "-fc="):
			patterns, err := loadPatternsFromFile(strings.TrimPrefix(arg, "-fc="))
			if err != nil {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, err
			}
			for _, p := range patterns {
				softFilters = append(softFilters, p)
			}
		case arg == "-Fc":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("-Fc 需要一个文件路径")
			}
			i++
			patterns, err := loadPatternsFromFile(args[i])
			if err != nil {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, err
			}
			for _, p := range patterns {
				hardFilters = append(hardFilters, p)
			}
		case strings.HasPrefix(arg, "-Fc="):
			patterns, err := loadPatternsFromFile(strings.TrimPrefix(arg, "-Fc="))
			if err != nil {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, err
			}
			for _, p := range patterns {
				hardFilters = append(hardFilters, p)
			}
		case arg == "--dir" || arg == "-d":
			consumed := 0
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				dirs.Set(args[i])
				consumed++
			}
			if consumed == 0 {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--dir 需要一个路径")
			}
		case strings.HasPrefix(arg, "--dir="):
			dirs.Set(strings.TrimPrefix(arg, "--dir="))
		case arg == "--filter" || arg == "-filter" || arg == "-f":
			consumed := 0
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				softFilters.Set(args[i])
				consumed++
			}
			if consumed == 0 {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--filter 需要一个表达式")
			}
		case strings.HasPrefix(arg, "--filter=") || strings.HasPrefix(arg, "-filter="):
			softFilters.Set(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "filter="))
		case arg == "--Filter" || arg == "-Filter" || arg == "-F":
			consumed := 0
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				hardFilters.Set(args[i])
				consumed++
			}
			if consumed == 0 {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--Filter 需要一个表达式")
			}
		case strings.HasPrefix(arg, "--Filter=") || strings.HasPrefix(arg, "-Filter="):
			hardFilters.Set(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "Filter="))
		case arg == "--out" || arg == "-o":
			if i+1 >= len(args) {
				return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--out 需要一个路径")
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

	if install && uninstall {
		return dirs, softFilters, hardFilters, out, help, install, uninstall, fmt.Errorf("--install 与 --uninstall 不能同时使用")
	}

	for _, arg := range leftover {
		if strings.HasPrefix(arg, "!") || strings.ContainsAny(arg, "*?[]") {
			softFilters.Set(arg)
			continue
		}
		dirs.Set(arg)
	}
	return dirs, softFilters, hardFilters, out, help, install, uninstall, nil
}

func normalizeFilters(filters []string) []string {
	var out []string
	for _, f := range filters {
		if f == "" {
			continue
		}
		f = strings.ReplaceAll(f, "\\", "/")
		if strings.HasSuffix(f, "/*") {
			base := strings.TrimSuffix(f, "/*")
			f = base + "/*"
		} else {
			f = strings.TrimSuffix(f, "/")
		}
		out = append(out, f)
	}
	return out
}

func loadPatternsFromFile(filePath string) ([]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("无法读取配置文件 %s: %w", filePath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var patterns []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", filePath, err)
	}
	return patterns, nil
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
		fmt.Fprintf(flag.CommandLine.Output(), "  --config/-c   指定配置文件路径 (默认作为软过滤); 行首 # 视为注释\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  -fc           指定配置文件路径 (强制作为软过滤); 行首 # 视为注释\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  -Fc           指定配置文件路径 (强制作为硬过滤); 行首 # 视为注释\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  Pattern 语法: ? 单字符 (test?.log); * 任意串 (*.go); [] 字符范围 (file[0-9].txt); 前缀 ! 取反 (!important.txt)\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --out/-o      指定输出文件路径或输出目录\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --no-fold     在目录树中不折叠长文件列表，始终显示全部文件 (默认超过 %d 个文件折叠)\n", maxDisplayFiles)
		fmt.Fprintf(flag.CommandLine.Output(), "  --install     安装程序到系统 (Linux: /usr/local/bin; Windows: Program Files 并添加 PATH)\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --uninstall   从系统中卸载程序\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  位置参数      未被 --dir 消耗的参数：若含 * ? [] 或以 ! 开头视为软过滤，其它视为目录\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  --help/-h     显示此帮助\n")
	}

	parsedDirs, parsedSoftFilters, parsedHardFilters, outFlag, help, install, uninstall, err := parseCommandLine()
	if help {
		flag.Usage()
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		flag.Usage()
		os.Exit(1)
	}

	if install {
		if err := manageInstallation(true); err != nil {
			fmt.Fprintf(os.Stderr, "安装失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if uninstall {
		if err := manageInstallation(false); err != nil {
			fmt.Fprintf(os.Stderr, "卸载失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	dirs := []string(parsedDirs)
	softFilters := normalizeFilters([]string(parsedSoftFilters))
	hardFilters := normalizeFilters([]string(parsedHardFilters))
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

			if relSlash != "" {
				matchedHard, _ := checkFilter(relSlash, hardFilters)
				if matchedHard {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}

			matchedSoft, rule := checkFilter(relSlash, softFilters)
			if matchedSoft {
				display := relSlash
				if display == "" {
					display = filepath.ToSlash(fullPath)
				}
				if d.IsDir() {
					fmt.Printf("[SKIP] 忽略目录 (Soft Filter: \"%s\"): %s\n", rule, display)
					return filepath.SkipDir
				}
				fmt.Printf("[SKIP] 忽略内容 (Soft Filter: \"%s\"): %s\n", rule, display)
				return nil
			}

			if d.IsDir() {
				return nil
			}

			if isAsset(name) {
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

func manageInstallation(isInstall bool) error {
	if runtime.GOOS == "windows" {
		return manageWindows(isInstall)
	}
	return manageUnix(isInstall)
}

func manageUnix(isInstall bool) error {
	targetDir := "/usr/local/bin"
	targetName := "dir2txt"
	targetPath := filepath.Join(targetDir, targetName)

	if !isInstall {
		fmt.Printf("正在卸载: %s\n", targetPath)
		if err := os.Remove(targetPath); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("未找到已安装的程序")
			}
			return fmt.Errorf("卸载失败 (权限不足?): %v", err)
		}
		fmt.Println("[SUCCESS] 卸载成功")
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		realPath = exePath
	}

	fmt.Printf("正在安装: %s -> %s\n", realPath, targetPath)

	srcFile, err := os.Open(realPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(targetPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("无法写入目标路径 (请尝试 sudo): %v", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	fmt.Println("[SUCCESS] 安装成功！现在可以在任意位置运行 dir2txt")
	return nil
}

func manageWindows(isInstall bool) error {
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\\Program Files`
	}
	installDir := filepath.Join(programFiles, "dir2txt")
	targetExe := filepath.Join(installDir, "dir2txt.exe")

	if !isInstall {
		fmt.Printf("正在移除文件: %s\n", targetExe)
		os.Remove(targetExe)
		os.Remove(installDir)
		fmt.Println("[SUCCESS] 文件已移除。")
		fmt.Println("[WARNING]  注意: 为了安全起见，程序不会自动修改注册表。请手动从环境变量 PATH 中删除该路径。")
		return nil
	}

	fmt.Printf("正在安装到: %s\n", installDir)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("无法创建目录 (请以管理员身份运行): %v", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	srcFile, err := os.Open(exePath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(targetExe, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("无法写入文件 (请以管理员身份运行): %v", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	fmt.Println("[SUCCESS] 文件复制成功。")

	fmt.Println("正在配置环境变量...")

	psScript := fmt.Sprintf(`
		$target = "%s"
		$currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
		if ($currentPath -like "*$target*") {
			Write-Host "环境变量已存在，跳过。"
		} else {
			$newPath = $currentPath + ";$target"
			[Environment]::SetEnvironmentVariable("Path", $newPath, "User")
			Write-Host "环境变量已更新。"
		}
	`, installDir)

	cmd := exec.Command("powershell", "-Command", psScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("[WARNING] 环境变量自动设置失败: %v\n详情: %s\n请手动将 %s 添加到 PATH\n", err, string(output), installDir)
	} else {
		fmt.Print(string(output))
		fmt.Println("安装完成！请重启终端以生效。")
	}

	return nil
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

// checkFilter 检查路径是否命中过滤规则，返回是否匹配以及命中的原始规则
// 规则：
// - dir 或 dir/ : 目录前缀匹配，目录本身和其子孙均命中
// - dir/*       : 目录下的内容命中，目录本身不命中（保留空目录）
// - glob        : 尝试匹配全路径或文件名
// - ! 前缀      : 取反（豁免）
func checkFilter(fullPath string, filters []string) (bool, string) {
	if fullPath == "" {
		return false, ""
	}

	full := filepath.ToSlash(fullPath)

	for _, rule := range filters {
		if rule == "" {
			continue
		}

		isNeg := strings.HasPrefix(rule, "!")
		cleanRule := strings.TrimPrefix(rule, "!")
		cleanRule = filepath.ToSlash(cleanRule)

		matched := false

		if strings.HasSuffix(cleanRule, "/*") {
			parent := strings.TrimSuffix(cleanRule, "/*")
			if parent != "" && strings.HasPrefix(full, parent+"/") && full != parent {
				matched = true
			}
		} else {
			cleanRule = strings.TrimSuffix(cleanRule, "/")

			if cleanRule != "" && (full == cleanRule || strings.HasPrefix(full, cleanRule+"/")) {
				matched = true
			} else {
				if m, _ := path.Match(cleanRule, full); m {
					matched = true
				}
				if m, _ := path.Match(cleanRule, filepath.Base(full)); m {
					matched = true
				}
			}
		}

		if matched {
			if isNeg {
				return false, rule
			}
			return true, rule
		}
	}

	return false, ""
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
		if relSlash != "" {
			matched, _ := checkFilter(relSlash, hardFilters)
			if matched {
				// 目录层保留，但被匹配的子节点会被隐藏
				continue
			}
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

	if !config.NoFold && len(files) > maxDisplayFiles {
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
