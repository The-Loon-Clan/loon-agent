package services

import (
	"os/exec"
	"strings"
	"sync"
)

// AI upscale model catalog. The set is intentionally data-driven: add a
// row + bundle the (few-MB) model file in the GPU agent image and the
// model becomes selectable; retire a loser by deleting its row. The
// agent advertises every model whose upscaler binary is on PATH so the
// site's request dropdown only ever offers models some capable agent
// can actually run — letting us iterate on which model wins over time
// without a site migration.
//
// This registry is the single source of truth shared by capability
// advertising (AvailableUpscaleModels, below) and the runtime upscale
// pipeline (services.RunUpscale, Phase 2).
type UpscaleModel struct {
	Key     string // request option key, e.g. "upscale_anime_2x"
	Label   string // human label for the request dropdown
	Binary  string // ncnn-vulkan CLI this model runs under
	Scale   int    // -s output scale factor
	Content string // "anime" | "manga" | "general" — match to source
	// Type routes the dispatcher: "video" runs the chunked ffmpeg
	// pipeline; "image" runs the per-page CBZ pipeline (extract pages
	// → ncnn dir-mode → repack). Defaults to "video" when omitted so
	// existing video-only entries keep working.
	Type string
	// Args is the model-specific arg slice appended after the standard
	// -i/-o/-s/-f flags. Per-binary because realesrgan-ncnn-vulkan takes
	// `-n <model>` while realcugan-ncnn-vulkan takes `-m <models_dir>`
	// (and supports a `-n <noise_level>` tuning knob); keeping the args
	// as data lets us add either family — or future tuning options —
	// with a one-line registry change.
	Args []string
}

// upscaleModelRegistry starts deliberately small. The exact Binary /
// Model / Scale strings are finalised against the GPU image in Phase 2;
// for now they drive capability advertising (binary-presence probe).
var upscaleModelRegistry = []UpscaleModel{
	// RealCUGAN, denoise level 1 — good default for noisy old TV/DVD anime.
	{Key: "upscale_anime_2x", Label: "Anime 2x (RealCUGAN, denoise)",
		Binary: "realcugan-ncnn-vulkan", Scale: 2, Content: "anime",
		Args: []string{"-m", "models-se", "-n", "1"}},
	// Real-ESRGAN animevideov3 — for already-clean anime where RealCUGAN's
	// denoise would over-smooth line art.
	{Key: "upscale_anime_clean_2x", Label: "Anime 2x (Real-ESRGAN animevideov3)",
		Binary: "realesrgan-ncnn-vulkan", Scale: 2, Content: "anime",
		Args: []string{"-n", "realesr-animevideov3"}},
	// Real-ESRGAN general-x4v3 — live action / photographic content where
	// anime models look waxy.
	{Key: "upscale_general_2x", Label: "Live action / general 2x (Real-ESRGAN)",
		Binary: "realesrgan-ncnn-vulkan", Scale: 2, Content: "general",
		Args: []string{"-n", "realesr-general-x4v3"}},
	// ─── Manga (image / CBZ) ────────────────────────────────────────
	// Drives runImageUpscale on CBZ archives: extract pages → ncnn
	// dir-mode → repack. No ffmpeg, no chunking — pages are independent
	// and tiny compared to video frames, so a chapter finishes in
	// seconds on a 5090.
	{Key: "upscale_manga_2x", Label: "Manga 2x (RealCUGAN, denoise)",
		Binary: "realcugan-ncnn-vulkan", Scale: 2, Content: "manga", Type: "image",
		Args: []string{"-m", "models-se", "-n", "1"}},
	// RealESRGAN_x4plus_anime_6B is the higher-quality stills model —
	// trained on anime illustration / line art, ideal for clean scan
	// raws and modern web-only manga.
	{Key: "upscale_manga_clean_2x", Label: "Manga 2x (Real-ESRGAN anime stills)",
		Binary: "realesrgan-ncnn-vulkan", Scale: 2, Content: "manga", Type: "image",
		Args: []string{"-n", "RealESRGAN_x4plus_anime_6B"}},
}

// UpscaleModelByKey returns the registry row for a request option key.
func UpscaleModelByKey(key string) (UpscaleModel, bool) {
	for _, m := range upscaleModelRegistry {
		if m.Key == key {
			return m, true
		}
	}
	return UpscaleModel{}, false
}

// AvailableUpscaleModels returns the keys of every registry model whose
// upscaler binary is on PATH — exactly what this agent advertises to
// the site. nil on a CPU-only agent (no upscaler binaries installed),
// which keeps such an agent out of the request dropdown automatically.
func AvailableUpscaleModels() []string {
	binOnPath := map[string]bool{}
	var keys []string
	for _, m := range upscaleModelRegistry {
		ok, seen := binOnPath[m.Binary]
		if !seen {
			_, err := exec.LookPath(m.Binary)
			ok = err == nil
			binOnPath[m.Binary] = ok
		}
		if ok {
			keys = append(keys, m.Key)
		}
	}
	return keys
}

var (
	gpuCapOnce    sync.Once
	gpuInfoCache  string
	gpuModelCache []string
)

// GPUCapabilities returns the (GPU description, advertised model keys)
// for this agent, detected once on first call and cached thereafter —
// so the per-poll capability report can call it without re-spawning
// nvidia-smi every time.
func GPUCapabilities() (string, []string) {
	gpuCapOnce.Do(func() {
		gpuInfoCache = DetectGPU()
		gpuModelCache = AvailableUpscaleModels()
	})
	return gpuInfoCache, gpuModelCache
}

// DetectGPU returns a human GPU description via nvidia-smi (e.g.
// "NVIDIA GeForce RTX 5090 (32760 MiB)"), or "" when no NVIDIA GPU /
// driver is present. Best-effort: any error → "". Cheap enough to call
// once at startup; do not call per-poll.
func DetectGPU() string {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total", "--format=csv,noheader")
	cmd.Env = toolEnv()
	out, err := cmd.Output()
	if err != nil {
		escalateToolCrash("nvidia-smi", "", out, err)
		return ""
	}
	line := strings.TrimSpace(out2first(string(out)))
	if line == "" {
		return ""
	}
	// "NVIDIA GeForce RTX 5090, 32760 MiB" → "NVIDIA GeForce RTX 5090 (32760 MiB)"
	if name, mem, ok := strings.Cut(line, ","); ok {
		return strings.TrimSpace(name) + " (" + strings.TrimSpace(mem) + ")"
	}
	return line
}

func out2first(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
