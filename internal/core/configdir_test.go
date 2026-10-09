package core

import (
	"path/filepath"
	"testing"
)

func TestAppBundleParent(t *testing.T) {
	for exe, want := range map[string]string{
		"/Applications/FTP App.app/Contents/MacOS/FTP_App": "/Applications",
		"/Users/me/Tools/FTP_App.app/Contents/MacOS/x":     "/Users/me/Tools",
	} {
		got, ok := appBundleParent(filepath.FromSlash(exe))
		if !ok || filepath.ToSlash(got) != want {
			t.Errorf("appBundleParent(%q) = %q, %v, want %q", exe, got, ok, want)
		}
	}
	for _, exe := range []string{
		"/home/me/ftpapp/FTP_App",
		"/usr/local/bin/FTP_App",
		"/Applications/Thing.app/Contents/Resources/FTP_App",
		"/Applications/MacOS/Contents/FTP_App",
		"C:/tools/FTP_App.exe",
		"FTP_App",
	} {
		if got, ok := appBundleParent(filepath.FromSlash(exe)); ok {
			t.Errorf("%q is not inside an app bundle, got %q", exe, got)
		}
	}
}
