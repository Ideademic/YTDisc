package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "YTDisc",
		Width:     1280,
		Height:    800,
		MinWidth:  900,
		MinHeight: 500,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Custom HTTP handler for /video/* and /thumb/* — must
			// support HTTP Range so the <video> element can seek.
			Handler: NewMediaHandler(app),
		},
		BackgroundColour: &options.RGBA{R: 26, G: 19, B: 16, A: 1},
		OnStartup:        app.startup,
		Bind: []interface{}{
			app,
		},
		Mac: &mac.Options{
			WebviewIsTransparent: false,
			// WKWebView ships with the JavaScript fullscreen API
			// disabled by default on macOS — without this preference,
			// requestFullscreen() throws silently and the native
			// video-control fullscreen button hides itself.
			Preferences: &mac.Preferences{
				FullscreenEnabled: mac.Enabled,
			},
			About: &mac.AboutInfo{
				Title:   "YTDisc",
				Message: "Portable YouTube library player",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}