package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	sdkRoot := os.Getenv("ANDROID_HOME")
	if sdkRoot == "" {
		sdkRoot = os.Getenv("ANDROID_SDK_ROOT")
	}
	requirements := []struct {
		name string
		ok   bool
	}{
		{"Go", commandAvailable("go", "version")},
		{"Node.js", commandAvailable("node", "--version")},
		{"npm", commandAvailable("npm", "--version")},
		{"JDK", commandAvailable("java", "-version")},
		{"Task", commandAvailable("task", "--version")},
		{"Wails v3", commandAvailable("wails3", "version")},
		{"ANDROID_HOME", sdkRoot != ""},
		{"Android API 35", directoryExists(filepath.Join(sdkRoot, "platforms", "android-35"))},
		{"Build Tools 35.0.0", directoryExists(filepath.Join(sdkRoot, "build-tools", "35.0.0"))},
		{"Platform Tools", directoryExists(filepath.Join(sdkRoot, "platform-tools"))},
		{"NDK 26.3.11579264", directoryExists(filepath.Join(sdkRoot, "ndk", "26.3.11579264"))},
	}
	missing := false
	for _, requirement := range requirements {
		if requirement.ok {
			fmt.Printf("✓ %s\n", requirement.name)
			continue
		}
		missing = true
		fmt.Printf("✗ %s\n", requirement.name)
	}
	if missing {
		os.Exit(1)
	}
	fmt.Println("Android build toolchain is ready; no emulator is required.")
}

func commandAvailable(name string, arguments ...string) bool {
	command := exec.Command(name, arguments...)
	command.Stdout = nil
	command.Stderr = nil
	return command.Run() == nil
}

func directoryExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
