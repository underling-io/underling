package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

// streamEvent is the part of a stream-json event underling reads.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// initEvent is the part of the system/init event underling checks. Field
// names follow the Agent SDK's SDKSystemMessage, which stream-json emits.
type initEvent struct {
	PermissionMode string       `json:"permissionMode"`
	Plugins        []initPlugin `json:"plugins"`
	// PluginErrors is absent when every plugin loaded.
	PluginErrors []json.RawMessage `json:"plugin_errors"`
}

type initPlugin struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// streamOutput is what underling takes from claude's output.
type streamOutput struct {
	init    *initEvent // the first system/init event, if any
	summary string     // the last event's result text
	ok      bool       // whether the last event is a successful result
}

// readStream reads stream-json output to its end, skipping lines that
// aren't JSON events. Lines may be of any length.
func readStream(r io.Reader) (streamOutput, error) {
	br := bufio.NewReader(r)
	var out streamOutput
	var last streamEvent
	for {
		line, readErr := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var e streamEvent
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				last = e
				if out.init == nil && e.Type == "system" && e.Subtype == "init" {
					out.init = new(initEvent)
					_ = json.Unmarshal(line, out.init)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return out, readErr
		}
	}
	if last.Type == "result" {
		out.summary, out.ok = last.Result, !last.IsError
	}
	return out, nil
}

// checkInit checks that the session started as configured: in the
// permission mode it was started with, a plugin loaded from each of
// pluginDirs, and no plugin errors. -p
// silently ignores invalid settings, so the init event is the only proof
// that --settings and --plugin-dir took effect.
func checkInit(init *initEvent, mode string, pluginDirs []string) error {
	if init == nil {
		return errors.New("no system/init event")
	}
	if init.PermissionMode != mode {
		return fmt.Errorf("permission mode %q, not %s", init.PermissionMode, mode)
	}
	for _, dir := range pluginDirs {
		if !slices.ContainsFunc(init.Plugins, func(p initPlugin) bool { return loadedFrom(p.Path, dir) }) {
			return fmt.Errorf("no plugin loaded from %s", dir)
		}
	}
	if len(init.PluginErrors) > 0 {
		msgs := make([]string, len(init.PluginErrors))
		for i, e := range init.PluginErrors {
			msgs[i] = string(e)
		}
		return fmt.Errorf("plugin errors: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// loadedFrom reports whether a plugin at path came from the --plugin-dir
// dir: the dir itself, or a plugin in a folder of plugins.
func loadedFrom(path, dir string) bool {
	return Within(filepath.Clean(path), filepath.Clean(dir)) || Within(ResolvePath(path), ResolvePath(dir))
}
