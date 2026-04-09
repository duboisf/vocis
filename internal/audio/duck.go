package audio

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"vocis/internal/sessionlog"
)

// Ducker lowers the default audio sink volume while recording and restores it after.
type Ducker struct {
	savedVolumes map[string]string
	duckLevel    float64
}

// NewDucker creates a ducker that will lower the default sink to the given level (0.0–1.0).
// A duckLevel of 0 disables ducking.
func NewDucker(duckLevel float64) *Ducker {
	return &Ducker{duckLevel: duckLevel}
}

// Duck saves the current volume of all sinks and lowers them.
func (d *Ducker) Duck() {
	if d.duckLevel <= 0 {
		return
	}

	sinks := listSinkIDs()
	if len(sinks) == 0 {
		sessionlog.Warnf("duck: no sinks found")
		return
	}

	d.savedVolumes = make(map[string]string, len(sinks))
	for _, id := range sinks {
		out, err := exec.Command("wpctl", "get-volume", id).Output()
		if err != nil {
			continue
		}
		vol := parseVolume(strings.TrimSpace(string(out)))
		if vol == "" || parseFloat(vol) <= d.duckLevel {
			continue
		}
		d.savedVolumes[id] = vol
		if err := exec.Command("wpctl", "set-volume", id, fmt.Sprintf("%.2f", d.duckLevel)).Run(); err != nil {
			sessionlog.Warnf("duck: failed to lower sink %s: %v", id, err)
			delete(d.savedVolumes, id)
			continue
		}
		sessionlog.Infof("ducked sink=%s volume=%s → %.0f%%", id, vol, d.duckLevel*100)
	}
}

// Restore returns all ducked sinks to their pre-duck levels.
func (d *Ducker) Restore() {
	for id, vol := range d.savedVolumes {
		if err := exec.Command("wpctl", "set-volume", id, vol).Run(); err != nil {
			sessionlog.Warnf("duck: failed to restore sink %s: %v", id, err)
			continue
		}
		sessionlog.Infof("restored sink=%s volume=%s", id, vol)
	}
	d.savedVolumes = nil
}

// listSinkIDs returns wpctl IDs for all audio sinks.
func listSinkIDs() []string {
	out, err := exec.Command("wpctl", "status").Output()
	if err != nil {
		return nil
	}
	var ids []string
	inSinks := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "Sinks:") {
			inSinks = true
			continue
		}
		if inSinks {
			if trimmed == "" || strings.Contains(trimmed, ":") && !strings.Contains(trimmed, "[vol:") {
				break
			}
			// Lines look like: "*   47. Built-in Audio Analog Stereo        [vol: 0.84]"
			// or:               "   118. Dell D3100 ..."
			cleaned := strings.TrimLeft(trimmed, "│ *")
			cleaned = strings.TrimSpace(cleaned)
			if dot := strings.Index(cleaned, "."); dot > 0 {
				id := strings.TrimSpace(cleaned[:dot])
				if _, err := strconv.Atoi(id); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// parseVolume extracts the numeric value from wpctl output like "Volume: 0.84".
func parseVolume(output string) string {
	parts := strings.Fields(output)
	for i, part := range parts {
		if part == "Volume:" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
