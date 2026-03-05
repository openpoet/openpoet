//go:build !windows

package session

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// unixPTY wraps a Unix pseudo-terminal master file descriptor.
type unixPTY struct {
	ptmx *os.File
}

func startPTY(cmd *exec.Cmd, rows, cols uint16) (ptyHandle, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	h := &unixPTY{ptmx: ptmx}
	h.Resize(rows, cols)
	return h, nil
}

func (p *unixPTY) Read(buf []byte) (int, error)  { return p.ptmx.Read(buf) }
func (p *unixPTY) Write(buf []byte) (int, error) { return p.ptmx.Write(buf) }
func (p *unixPTY) Close() error                  { return p.ptmx.Close() }

func (p *unixPTY) Resize(rows, cols uint16) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}
