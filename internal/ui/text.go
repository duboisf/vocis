package ui

import "strings"

// Shorten truncates text to max runes, appending "..." if truncated.
func Shorten(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// WrapLines splits text into lines that fit within maxChars, preserving
// explicit newlines. Each paragraph is word-wrapped independently.
func WrapLines(text string, maxChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, wrapParagraph(paragraph, maxChars)...)
	}
	return lines
}

func wrapParagraph(text string, maxChars int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := words[0]
	for _, word := range words[1:] {
		if len([]rune(current))+1+len([]rune(word)) > maxChars {
			lines = append(lines, current)
			current = word
		} else {
			current += " " + word
		}
	}
	lines = append(lines, current)
	return lines
}

// TextLimit computes how many characters fit in the body area.
func TextLimit(width, rightPadding, glyphWidth int) int {
	const (
		textLeft = 150
		minChars = 12
	)
	if glyphWidth <= 0 {
		glyphWidth = 7
	}
	available := width - textLeft - rightPadding
	if available <= 0 {
		return minChars
	}
	chars := available / glyphWidth
	if chars < minChars {
		return minChars
	}
	return chars
}

// ListeningBody returns the body text for the listening state.
func ListeningBody(text string) string {
	text = NormalizeListeningText(text)
	if text == "" {
		return ""
	}
	return text
}

// NormalizeListeningText trims whitespace from listening text.
func NormalizeListeningText(text string) string {
	return strings.TrimSpace(text)
}

// DisplayedListeningText extracts the actual transcribed text from the body,
// returning empty if only the helper body is shown.
func DisplayedListeningText(body string) string {
	text := NormalizeListeningText(body)
	if text == NormalizeListeningText(ListeningBody("")) {
		return ""
	}
	return text
}

// ShouldAnimatePartial returns true when the new partial text extends
// the current text (appended words), indicating smooth animation.
func ShouldAnimatePartial(current, target string) bool {
	if target == "" {
		return false
	}
	if current == "" {
		return true
	}
	return strings.HasPrefix(target, current)
}

// NextWordBoundary finds the next word boundary in runes starting from start.
func NextWordBoundary(runes []rune, start int) int {
	i := start
	for i < len(runes) && runes[i] == ' ' {
		i++
	}
	for i < len(runes) && runes[i] != ' ' {
		i++
	}
	return i
}
