package main

import (
	"embed"
	"os"
	goruntime "runtime"

	"ftpapp/internal/core"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func goos() string { return goruntime.GOOS }

func main() {
	dir, err := core.ConfigDir()
	if err != nil {
		showFatal("Cannot find a folder for the settings:\n" + err.Error())
		os.Exit(1)
	}
	mgr, err := core.NewManager(dir)
	if err != nil {
		showFatal("The settings in " + dir + " cannot be used:\n\n" + err.Error() +
			"\n\nFix or remove the file named above and start the app again.")
		os.Exit(1)
	}
	app := NewApp(mgr)

	err = wails.Run(&options.App{
		Title:     "FTP App",
		Width:     1120,
		Height:    740,
		MinWidth:  940,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 20, G: 23, B: 28, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		DragAndDrop:      &options.DragAndDrop{EnableFileDrop: true},
		// The UI is plain text and boxes, so GPU compositing buys nothing; without it WebView2 needs
		// about 45 MB less. Set FTPAPP_GPU=1 to switch hardware acceleration back on.
		Windows: &windows.Options{Theme: windows.SystemDefault, WebviewGpuIsDisabled: os.Getenv("FTPAPP_GPU") != "1"},
		Bind:    []interface{}{app},
	})
	if err != nil {
		showFatal(err.Error())
		os.Exit(1)
	}
}
