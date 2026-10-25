package config

import (
	"encoding/json"
	"os"
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

// loadFile 读取并解析 config.json；文件不存在或解析失败时返回空 map（静默降级到默认值）。
func loadFile() map[string]string {
	result := make(map[string]string)

	path := defaultConfigPath
	if v := strings.TrimSpace(os.Getenv("CONFIG_PATH")); v != "" {
		path = v
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
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
	return result
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
