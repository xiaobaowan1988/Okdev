// Package deployer 是 platform/deployer 的最小 HTTP 客户端:网关只需要按名字查构建器(取仓库地址列分支)。
// 其余请求由网关原样转发,不经过这里。
package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError 是 deployer 返回的非 2xx 响应。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("deployer: %d %s", e.Status, e.Message) }

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Builder 只取列分支需要的字段;构建脚本和 Dockerfile 的内容不读取。
type Builder struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	RepoURL string `json:"repo_url"`
	Ref     string `json:"ref"`
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) GetBuilder(ctx context.Context, name string) (*Builder, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/builders/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	var b Builder
	return &b, json.Unmarshal(data, &b)
}
