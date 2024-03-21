package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SoMuchForSubtlety/opendj"
)

func fmtDuration(d time.Duration) string {
	return d.String()
}

func durationBar(width int, fraction time.Duration, total time.Duration) string {
	base := strings.Repeat("—", width)
	pos := float64(fraction.Seconds() / total.Seconds())

	newpos := int(float64(utf8.RuneCountInString(base))*pos) - 1
	if newpos < 0 {
		newpos = 0
	} else if newpos >= utf8.RuneCountInString(base) {
		newpos = utf8.RuneCountInString(base) - 1
	}

	return replaceAtIndex(base, '⚫', newpos)
}

func replaceAtIndex(in string, r rune, i int) string {
	out := ""
	j := 0
	for _, c := range in {
		if j != i {
			out += string(c)
		} else {
			out += string(r)
		}
		j++
	}
	return out
}

func formatPlaylist(queue []opendj.QueueEntry, currentEntry opendj.QueueEntry) string {
	lines := fmt.Sprintf(" curretly playing: 🎶 %q 🎶 requested by %s\n\n", currentEntry.Media.Title, currentEntry.Owner)
	var maxname int
	for _, vid := range queue {
		if len(vid.Owner) > maxname {
			maxname = len(vid.Owner)
		}
	}
	for i, vid := range queue {
		lines += fmt.Sprintf(" %2v | %-*v | %v | %v\n", i+1, maxname, vid.Owner, fmtDuration(vid.Media.Duration), vid.Media.Title)
	}
	return lines
}
