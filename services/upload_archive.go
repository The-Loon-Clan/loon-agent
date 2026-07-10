package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// GenerateRandomPassword creates a hex string of the given byte length.
// crypto/rand is the primary source; if it fails (effectively never on
// real kernels, but possible on broken /dev/urandom or some sandboxed
// builds) we fall back to a time-seeded math/rand sequence and log the
// fallback so the operator can investigate. Calling code never panics —
// this function is used in dir-name generation paths that must not
// take the agent down.
//
// For 7z archive passwords the fallback path is still adequate: the
// security model relies on the user not sharing the password, not on
// the password being unguessable from a kernel-RNG-deprived attacker.
// Real concern would be two simultaneous fallback calls producing the
// same byte sequence; the time-nanosecond seed guards against that.
func GenerateRandomPassword(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		log.Printf("crypto/rand failed, using time-seeded fallback for %d-byte token: %v", length, err)
		src := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
		for i := range bytes {
			bytes[i] = byte(src.Intn(256))
		}
	}
	return hex.EncodeToString(bytes)
}

// EncryptWith7z creates a password-protected 7z archive with header encryption.
// This encrypts both file data AND filenames so the contents are completely opaque.
// SABnzbd and NZBGet both support automatic extraction when the password is
// provided via NZB <meta type="password"> header.
//
// The -mhe=on flag enables header encryption (filenames hidden).
// The -mx=0 flag stores without compression (files are already compressed video).
func EncryptWith7z(ctx context.Context, srcDir, destPath, password string) error {
	// 7z a -p<password> -mhe=on -mx=0 -mmt=on output.7z <each entry>
	//
	// Enumerate srcDir explicitly and pass each entry as its own argv
	// element rather than relying on 7z's "*" glob expansion. 7z's
	// internal glob would treat any literal '*', '?', or '[' in a
	// filename as a metacharacter — extremely rare in practice but
	// possible on filenames carried over from a CJK source where the
	// transliteration produced one of those bytes. Argv-explicit also
	// makes the failure mode "this exact filename couldn't be added"
	// instead of "the glob silently matched the wrong thing".
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("7z: read srcDir %s: %w", srcDir, err)
	}
	args := []string{
		"a",
		"-p" + password,
		"-mhe=on", // encrypt headers (hide filenames)
		"-mx=0",   // store only, no compression (faster, video is already compressed)
		"-mmt=on", // multi-threaded — uses all available cores for hashing/encryption
		destPath,
	}
	for _, e := range entries {
		args = append(args, filepath.Join(srcDir, e.Name()))
	}
	cmd := exec.CommandContext(ctx, "7z", args...)
	cmd.Dir = srcDir
	cmd.Env = toolEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		escalateToolCrash("7z", srcDir, out, err)
		return fmt.Errorf("7z: %v: %s", err, out)
	}
	return nil
}
