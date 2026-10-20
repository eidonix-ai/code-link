package messages

const (
	estimateImageTokens  = 1600
	estimateMaxPDFTokens = 60_000
)

// estimateTokens is the prompt size of a JSON request body: ~4 characters
// per token for text, with embedded base64 media costed separately (a flat
// figure per image, bytes-based for PDFs) instead of by its base64 length.
func estimateTokens(b []byte) int {
	tokens := 0
	textBytes := len(b)
	start := -1
	for i := 0; i <= len(b); i++ {
		inRun := false
		if i < len(b) {
			c := b[i]
			inRun = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
				c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
		}
		if inRun && start < 0 {
			start = i
		} else if !inRun && start >= 0 {
			if n := i - start; n >= 2048 {
				textBytes -= n
				// "JVBERi" is "%PDF-" in base64.
				if n >= 6 && string(b[start:start+6]) == "JVBERi" {
					tokens += min(n*3/4/150, estimateMaxPDFTokens)
				} else {
					tokens += estimateImageTokens
				}
			}
			start = -1
		}
	}
	return tokens + textBytes/4
}
