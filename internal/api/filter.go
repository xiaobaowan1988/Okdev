package api

import "encoding/json"

// 构建配方的内容(构建脚本、Dockerfile、构建参数)只给管理员看:脚本是任意代码,构建参数里可能有不该外传的东西。
var sensitiveKeys = map[string]bool{"dockerfile": true, "script": true, "build_args": true}

// stripSensitive 递归去掉 JSON 里的敏感字段。不是 JSON 就原样返回。
func stripSensitive(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	out, err := json.Marshal(strip(v))
	if err != nil {
		return body
	}
	return out
}

func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if sensitiveKeys[k] {
				delete(t, k)
			} else {
				t[k] = strip(t[k])
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = strip(t[i])
		}
		return t
	}
	return v
}
