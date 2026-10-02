package hive

import "os"

// writeFile is os.WriteFile with mode 0644, kept here so the schema
// tests stay about the schema.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
