//go:build !darwin

package bundled

// On Windows and Linux there's no Gatekeeper-equivalent that blocks
// a freshly-written executable from running. The 0o755 mode set by
// os.WriteFile is sufficient on Unix-y filesystems; Windows ignores
// mode bits entirely. So this is a no-op.
func postExtractFixup(path string) error {
	_ = path
	return nil
}
