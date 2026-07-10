package services

import (
	_ "embed"
	"net/http"
)

// tokensCSS is the vendored copy of the site's canonical palette +
// font imports (indexer-site/web/static/css/tokens.css). Served by
// the local UI at /_shared/tokens.css so the agent's stylesheet can
// reference the same :root variables the site does, without needing
// a running site to fetch them from. When the site changes the
// palette, update both copies in lockstep — see the header comment
// inside tokens.css for the rationale.
//
//go:embed tokens.css
var tokensCSS []byte

// componentsCSS is the vendored copy of the site's shared component
// primitives (indexer-site/web/static/css/components.css). Holds the
// cross-surface atoms (.dot, .alert) that look identical on both the
// site and the agent. Layered on top of tokens.css and below
// localui.css, so the agent's layout can still override specifics if
// it needs to. Lockstep copy; see the file header for the rationale.
//
//go:embed components.css
var componentsCSS []byte

// agentShellCSS is the vendored copy of the site's agent-shell
// stylesheet (indexer-site/web/static/css/agent-shell.css). Holds
// the .local-shell-scoped card / status-row / speed-row / disk-bar
// primitives that both surfaces render identically. The agent's
// sidebar opts in by setting class="sidebar local-shell", so the
// rules apply inside the sidebar but don't bleed into the main
// content area. Lockstep copy; see the file header for the rationale.
//
//go:embed agent-shell.css
var agentShellCSS []byte

// localuiCSS is the agent-specific stylesheet — layout, components,
// and everything that isn't in tokens.css, components.css, or
// agent-shell.css. Kept as a real .css file (rather than a Go string
// literal in localui_templates.go) so editors get syntax highlighting
// and diffs are readable. Load order in the template must be
// tokens.css → components.css → agent-shell.css → localui.css.
//
//go:embed localui.css
var localuiCSS []byte

// cssHandler returns an http.HandlerFunc that serves a baked-in CSS
// blob with aggressive caching. The files are go:embedded at compile
// time so they can't change under a running process — `immutable`
// is safe.
func cssHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		_, _ = w.Write(body)
	}
}

// ServeTokensCSS serves /_shared/tokens.css.
var ServeTokensCSS = cssHandler(tokensCSS)

// ServeComponentsCSS serves /_shared/components.css.
var ServeComponentsCSS = cssHandler(componentsCSS)

// ServeAgentShellCSS serves /_shared/agent-shell.css.
var ServeAgentShellCSS = cssHandler(agentShellCSS)

// ServeLocalUICSS serves /static/localui.css.
var ServeLocalUICSS = cssHandler(localuiCSS)
