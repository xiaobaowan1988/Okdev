// Package deployer 是 platform/deployer 的 HTTP 客户端:环境、服务、构建器、流水线运行都由它管。
package deployer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError 是 deployer 返回的非 2xx 响应。4xx 说明请求本身有问题,重试没有意义。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("deployer: %d %s", e.Status, e.Message) }

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// ClientError 报告是不是 4xx(请求有问题)。
func ClientError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500
}

// 运行状态,取自 deployer 的状态机。
const (
	StatusPending   = "pending"
	StatusBuilding  = "building"
	StatusDeploying = "deploying"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

type Run struct {
	ID         int64      `json:"id"`
	Pipeline   string     `json:"pipeline"`
	Builder    string     `json:"builder"`
	Service    string     `json:"service"`
	Env        string     `json:"env"`
	Ref        string     `json:"ref"`
	Status     string     `json:"status"`
	Image      string     `json:"image"`
	Message    string     `json:"message"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Builder 只取界面和校验需要的字段;构建脚本和 Dockerfile 内容不读取。
type Builder struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	RepoURL string `json:"repo_url"`
	Ref     string `json:"ref"`
}

// EnvService 是服务在某个环境里的部署:期望状态由 API 写入,observed_* 由 deployer 的对账循环回写。
type EnvService struct {
	Env             string `json:"env"`
	Service         string `json:"service"`
	DesiredState    string `json:"desired_state"`
	Replicas        int    `json:"replicas"`
	Image           string `json:"image"`
	Version         int64  `json:"version"`
	AppliedVersion  int64  `json:"applied_version"`
	ObservedSync    string `json:"observed_sync"`
	ObservedHealth  string `json:"observed_health"`
	ObservedMessage string `json:"observed_message"`
}

// Service 是 deployer 里登记的服务(只取界面需要的字段)。
type Service struct {
	Name     string `json:"name"`
	Workload string `json:"workload"`
}

// Namespace 是 deployer 给 (环境, 服务) 起的命名空间,每个部署一个。
func Namespace(env, service string) string { return env + "-env-" + service }

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path, actor string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(data)
		return nil
	}
	return json.Unmarshal(data, out)
}

// Healthy 检查 deployer 是否可达。
func (c *Client) Healthy(ctx context.Context) error {
	var s string
	return c.do(ctx, http.MethodGet, "/healthz", "", nil, &s)
}

// CreateRun 运行流水线:构建成功后 deployer 会自动部署到所选环境。actor 记录在 deployer 的历史里。
func (c *Client) CreateRun(ctx context.Context, pipeline, builder, env, ref string, replicas *int, actor string) (*Run, error) {
	in := map[string]any{"builder": builder, "env": env}
	if ref != "" {
		in["ref"] = ref
	}
	if replicas != nil {
		in["replicas"] = *replicas
	}
	var run Run
	if err := c.do(ctx, http.MethodPost, "/v1/pipelines/"+url.PathEscape(pipeline)+"/runs", actor, in, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *Client) GetRun(ctx context.Context, id int64) (*Run, error) {
	var run Run
	if err := c.do(ctx, http.MethodGet, "/v1/runs/"+strconv.FormatInt(id, 10), "", nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// RunLog 返回运行的构建日志(纯文本):构建中是 Pod 的实时尾部,结束后是保存下来的。
func (c *Client) RunLog(ctx context.Context, id int64) (string, error) {
	var s string
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+strconv.FormatInt(id, 10)+"/log", "", nil, &s)
	return s, err
}

func (c *Client) GetBuilder(ctx context.Context, name string) (*Builder, error) {
	var b Builder
	if err := c.do(ctx, http.MethodGet, "/v1/builders/"+url.PathEscape(name), "", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) ListBuilders(ctx context.Context) ([]Builder, error) {
	var bs []Builder
	err := c.do(ctx, http.MethodGet, "/v1/builders", "", nil, &bs)
	return bs, err
}

func (c *Client) ListEnvs(ctx context.Context) ([]string, error) {
	var names []string
	err := c.do(ctx, http.MethodGet, "/v1/envs", "", nil, &names)
	return names, err
}

func (c *Client) ListEnvServices(ctx context.Context, env string) ([]EnvService, error) {
	var out []EnvService
	err := c.do(ctx, http.MethodGet, "/v1/envs/"+url.PathEscape(env)+"/services", "", nil, &out)
	return out, err
}

func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	var out []Service
	err := c.do(ctx, http.MethodGet, "/v1/services", "", nil, &out)
	return out, err
}
