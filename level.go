package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// loadObstacles reads an obstacle layout from a JSON file. The file is a plain
// array of rects, e.g. [{"x":250,"y":140,"w":130,"h":40}, ...].
func loadObstacles(path string) ([]rect, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var obs []rect
	if err := json.Unmarshal(data, &obs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return obs, nil
}

// loadLevels reads every *.json file in dir as a level, in filename order. The
// level name is the filename without extension. Returns an error if the dir
// can't be read or contains no usable levels.
func loadLevels(dir string) (levels [][]rect, names []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		obs, e := loadObstacles(filepath.Join(dir, f))
		if e != nil {
			return nil, nil, e
		}
		levels = append(levels, obs)
		names = append(names, strings.TrimSuffix(f, filepath.Ext(f)))
	}
	if len(levels) == 0 {
		return nil, nil, fmt.Errorf("no .json levels found in %s", dir)
	}
	return levels, names, nil
}
