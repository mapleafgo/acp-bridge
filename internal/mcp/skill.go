package mcp

import (
	"context"
	_ "embed"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// skillMD 是随二进制分发的 acp-bridge skill 正文。
// 通过 go:embed 内联编译产物，避免外部 skills 目录带来的双源分发问题。
//
// skill 同时作为 MCP Resource（URI: acp-bridge://skill）暴露给宿主，
// 宿主侧的 OrchestratorSkillProvider 可经 resources/list + resources/read
// 直接发现并读取，无需任何额外的 skill 目录、软链或配置。
//
//go:embed skill.md
var skillMD string

// skillResourceURI 是 skill 暴露为 MCP Resource 时使用的 URI。
// 使用 acp-bridge 自身 scheme，避免与 file:// 等宿主本地 scheme 冲突。
const skillResourceURI = "acp-bridge://skill"

// registerSkill 将内联的 skill.md 注册为 MCP Resource。
//
// 宿主侧的 OrchestratorSkillProvider 通过标准的 MCP resource 协议
// （resources/list 发现 → resources/read 读取）加载 skill 内容，
// 这样 skill 与 MCP 工具就共享同一个 stdio 连接，天然同源、同版本、
// 同权限边界，不再需要单独的 skills 目录或 Hermes 配置项。
func (s *Server) registerSkill() {
	s.sdkServer.AddResource(&sdk.Resource{
		URI:         skillResourceURI,
		Name:        "acp-bridge",
		Title:       "acp-bridge Skill",
		Description: skillResourceDescription(),
		MIMEType:    "text/markdown",
	}, skillResourceHandler)
}

// skillResourceDescription 从 skill.md 的 YAML frontmatter 提取 description，
// 保证 resource 的 description 与 skill 触发条件完全一致，避免双份维护。
func skillResourceDescription() string {
	desc := extractFrontmatterField(skillMD, "description")
	if desc == "" {
		// 兜底：frontmatter 缺失时不阻塞注册，用一句话说明用途。
		return "acp-bridge: bridge ACP-compatible agents (codex/claude/gemini) to MCP over stdio."
	}
	return desc
}

// skillResourceHandler 直接返回内联的 skill.md 全文。
// 因为 skill.md 已编译进二进制，这里不做任何文件 IO。
func skillResourceHandler(_ context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	return &sdk.ReadResourceResult{
		Contents: []*sdk.ResourceContents{
			{
				URI:      skillResourceURI,
				MIMEType: "text/markdown",
				Text:     skillMD,
			},
		},
	}, nil
}

// extractFrontmatterField 从 Markdown 文件的 YAML frontmatter 中提取指定字段。
// 仅做最简解析（键: 值，单行），不引入外部 YAML 依赖。
// 找不到字段或无 frontmatter 时返回空字符串。
func extractFrontmatterField(src, key string) string {
	const delim = "---"
	lines := splitLines(src)
	if len(lines) < 2 || lines[0] != delim {
		return ""
	}
	for _, ln := range lines[1:] {
		if ln == delim {
			break // frontmatter 结束
		}
		val, ok := parseKV(ln, key)
		if ok {
			return val
		}
	}
	return ""
}

// splitLines 按换行切分字符串，保留空行，去掉每行末尾的 \r。
func splitLines(src string) []string {
	var out []string
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			out = append(out, trimCR(src[start:i]))
			start = i + 1
		}
	}
	if start < len(src) {
		out = append(out, trimCR(src[start:]))
	}
	return out
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}

// parseKV 尝试把 "key: value" 形式的行解析为 (value, true)；
// 不是该 key 或不是 KV 行时返回 ("", false)。
func parseKV(line, key string) (string, bool) {
	prefix := key + ":"
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return "", false
	}
	rest := line[len(prefix):]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
		rest = rest[1 : len(rest)-1]
	}
	return rest, true
}
