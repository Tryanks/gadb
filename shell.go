package gadb

import (
	"io"
)

// Shell represents a running adb shell session started with a specific command.
//
// It allows streaming stdout/stderr and supports Close() to forcibly terminate
// the running remote command by closing the underlying socket (similar to Ctrl+C).
//
// Note: Writing to Stdin is not currently exposed; this is intended as a
// blocking runner with a force-stop capability.
type Shell struct {
	st shellTransport
	// stdout and stderr are multiplexed by the shell v2 protocol; callers can read from Reader.
	Reader io.Reader
}

// Close forcibly terminates the running remote shell command.
func (s *Shell) Close() error {
	return s.st.Close()
}

// internal helper to build a Reader that demultiplexes stdout/stderr messages
// from the shell transport and exposes a continuous stream of bytes.
func newShellReader(st *shellTransport) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			msgType, data, err := st.Read()
			if err != nil {
				// EOF or read error: terminate stream
				return
			}
			switch msgType {
			case shellStdout, shellStderr:
				if len(data) > 0 {
					if _, werr := pw.Write(data); werr != nil {
						return
					}
				}
			case shellExit:
				// exit code in data[0], we simply end the stream
				return
			case shellCloseStdin:
				// ignore for read side
			}
		}
	}()
	return pr
}
