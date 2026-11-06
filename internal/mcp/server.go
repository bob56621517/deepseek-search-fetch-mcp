// Package mcp 是 MCP 层：装配工具、Streamable HTTP 传输与健康检查。
package mcp

import (
	"context"
	"log/slog"
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bob56621517/deepseek-search-fetch-mcp/internal/openapi"
)

const serviceName = "deepseek-search-fetch-mcp"

// Options 是 MCP 层的装配参数。
type Options struct {
	Client  *openapi.Client
	Cache   *openapi.Cache
	Logger  *slog.Logger
	Version string
	// DefaultLang 非空时，搜索 query 未显式包含 lang: 会补全该语言码。
	DefaultLang string
}

// Server 持有 HTTP 处理器。
type Server struct {
	handler http.Handler
}

// New 构建 MCP 服务。
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	srv := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: serviceName, Version: opts.Version},
		&sdkmcp.ServerOptions{
			Capabilities: &sdkmcp.ServerCapabilities{Tools: &sdkmcp.ToolCapabilities{}},
			Logger:       logger,
		},
	)
	for _, tool := range openapi.Tools(opts.Client, opts.Cache, logger, opts.DefaultLang) {
		srv.AddTool(
			&sdkmcp.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
			},
			toolHandler(tool, logger),
		)
	}

	handler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return srv },
		&sdkmcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			Logger:       logger,
		},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)
	return &Server{handler: mux}
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	return s.handler
}

func toolHandler(tool openapi.Tool, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		body, err := tool.Call(ctx, req.Params.Arguments)
		if err != nil {
			logger.Warn("工具调用失败", "tool", tool.Name, "err", err)
			var res sdkmcp.CallToolResult
			res.SetError(err)
			return &res, nil
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(body)}},
		}, nil
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"service":"deepseek-search-fetch-mcp","status":"ok"}`))
}
