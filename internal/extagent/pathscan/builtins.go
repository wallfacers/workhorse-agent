package pathscan

// builtinAllowlist is the curated set of binary names to scan for on PATH.
// imagemagick is intentionally absent — it is a package name; the actual
// binaries are convert, magick, identify.
var builtinAllowlist = []string{
	"git", "gh", "jq", "yq", "curl", "wget", "rg", "fd",
	"pandoc", "libreoffice", "soffice",
	"ffmpeg", "convert", "magick", "identify", "yt-dlp",
	"playwright", "chromium", "chrome", "firefox",
	"python3", "node", "npm", "pnpm", "yarn", "deno", "bun",
	"go", "cargo", "rustc",
	"docker", "podman", "kubectl", "terraform", "ansible",
	"asciidoctor", "marp",
}

// Allowlist returns the resolved set of binary names to scan: builtin + extra - disabled.
func Allowlist(extra, disabled []string) []string {
	set := make(map[string]bool, len(builtinAllowlist)+len(extra))
	for _, name := range builtinAllowlist {
		set[name] = true
	}
	for _, name := range extra {
		set[name] = true
	}
	for _, name := range disabled {
		delete(set, name)
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}
