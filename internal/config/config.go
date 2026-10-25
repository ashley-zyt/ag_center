package config

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// 统一配置读取：优先级为「环境变量 > config.json > 默认值」。
//
// 背景：部署时逐个设置环境变量繁琐且易漏（尤其换机器），改为支持本地配置文件
// config.json（键名即环境变量名，如 "API_KEY"），一次配好、随项目拷贝即可。
// 环境变量仍然最高优先级，便于临时覆盖或兼容已有部署脚本。
//
// 使用方式：所有原本 os.Getenv("XXX") 的地方改为 config.Get("XXX", 默认值)。
// 文件采用懒加载（首次 Get 时读取一次），因此包级 init 里的读取也能正确拿到配置文件值。

// defaultConfigPath 默认配置文件路径；可用环境变量 CONFIG_PATH 覆盖。
const defaultConfigPath = "config.json"

var (
	loadOnce sync.Once
	fileVals map[string]string
)

// ensureLoaded 懒加载 config.json（进程内只加载一次）。
func ensureLoaded() {
	loadOnce.Do(func() {
		fileVals = loadFile()
	})
}

// loadFile 读取并解析 config.json；按候选路径逐个尝试，第一个能读到且解析成功的生效。
// 文件不存在时静默降级到默认值；文件存在但解析失败时打警告日志（配置写错了要能被发现）。
func loadFile() map[string]string {
	result := make(map[string]string)

	for _, path := range configPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 该候选不存在，试下一个
		}
		// 去掉 UTF-8 BOM：Windows 记事本/部分编辑器保存时会在文件头加 EF BB BF，
		// json.Unmarshal 不识别它，会报 "invalid character 'ï'"。
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			log.Printf("[CONFIG] 配置文件 %s 解析失败，已忽略（回退默认值）: %v", path, err)
			return result
		}

		for k, v := range raw {
			switch val := v.(type) {
			case string:
				result[k] = val
			case float64:
				result[k] = strconv.FormatFloat(val, 'f', -1, 64)
			case bool:
				result[k] = strconv.FormatBool(val)
			}
		}
		log.Printf("[CONFIG] 已加载配置文件: %s", path)
		return result
	}
	return result
}

// configPaths 返回候选配置文件路径：CONFIG_PATH > 工作目录 config.json > exe 所在目录 config.json。
// 最后一级兜底解决「从其它目录启动 exe 导致相对路径 config.json 找不到」的问题。
func configPaths() []string {
	var paths []string
	if v := strings.TrimSpace(os.Getenv("CONFIG_PATH")); v != "" {
		paths = append(paths, v)
	}
	paths = append(paths, defaultConfigPath) // 相对工作目录
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), defaultConfigPath))
	}
	return paths
}

// Get 返回字符串配置值：环境变量 > config.json > 默认值。
func Get(key, def string) string {
	ensureLoaded()

	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	if v, ok := fileVals[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// GetInt 返回整型配置值；无法解析时回退默认值。
func GetInt(key string, def int) int {
	v := strings.TrimSpace(Get(key, ""))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}
