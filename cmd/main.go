package main

import (
	// The time zone database inside the binary: the image is alpine without tzdata, and quiet hours and summaries are local times.
	_ "time/tzdata"

	app "github.com/pfisterer/notifications/internal"
)

func main() {
	app.Run()
}
