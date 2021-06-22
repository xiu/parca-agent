package buildid

import (
	"crypto/sha1"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/polarsignals/polarsignals-agent/byteorder"
	"github.com/polarsignals/polarsignals-agent/internal/pprof/elfexec"
)

func KernelBuildID() (string, error) {
	f, err := os.Open("/sys/kernel/notes")
	if err != nil {
		return "", err
	}

	notes, err := elfexec.ParseNotes(f, 4, byteorder.GetHostByteOrder())
	if err != nil {
		return "", err
	}

	for _, n := range notes {
		if n.Name == "GNU" {
			return fmt.Sprintf("%x", n.Desc), nil
		}
	}

	return "", errors.New("kernel build id not found")
}

func ElfBuildID(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}

	b, err := elfexec.GetBuildID(f)
	if err != nil {
		return "", err
	}

	if b == nil {
		// GNU build ID doesn't exist, so we hash the .text section. This
		// section typically contains the executable code.
		ef, err := elf.NewFile(f)
		if err != nil {
			return "", err
		}

		h := sha1.New()
		if _, err := io.Copy(h, ef.Section(".text").Open()); err != nil {
			return "", err
		}

		return hex.EncodeToString(h.Sum(nil)), nil
	}

	return hex.EncodeToString(b), nil
}