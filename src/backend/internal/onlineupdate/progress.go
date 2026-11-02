package onlineupdate

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DownloadProgress contains only measured layer transfer bytes; unknown totals stay zero.
type DownloadProgress struct{ DownloadedBytes, TotalBytes int64 }

// ProgressRunner optionally streams measured progress without changing legacy Runner mocks.
type ProgressRunner interface {
	RunProgress(context.Context, string, []string, []string, func(DownloadProgress)) error
}

type pullPhaseKey struct{}

var pullBytePattern = regexp.MustCompile(`^([a-f0-9]+):\s+Downloading.*?([0-9.]+)(B|kB|KB|MB|GB|KiB|MiB|GiB)/([0-9.]+)(B|kB|KB|MB|GB|KiB|MiB|GiB)\s*$`)

// pullBytes converts Docker's displayed transfer value to bytes, returning zero for invalid input.
func pullBytes(value, unit string) int64 {
	number, err := strconv.ParseFloat(value, 64)
	scale := map[string]float64{"B": 1, "kB": 1000, "KB": 1000, "MB": 1e6, "GB": 1e9, "KiB": 1024, "MiB": 1048576, "GiB": 1073741824}[unit]
	if err != nil || number < 0 || number*scale > 1e15 {
		return 0
	}
	return int64(number * scale)
}

// scanPullProgress reads bounded Docker lines and reports observed layer bytes, never invented totals.
// Non-terminal Docker output often omits sizes: no callback is then emitted and percent stays null.
func scanPullProgress(reader io.Reader, report func(DownloadProgress)) {
	layers := map[string]int64{}
	scanner := bufio.NewScanner(reader)
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if index := strings.IndexAny(string(data), "\r\n"); index >= 0 {
			return index + 1, data[:index], nil
		}
		if eof && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		fields := pullBytePattern.FindStringSubmatch(scanner.Text())
		if len(fields) != 6 || len(layers) > 4096 {
			continue
		}
		current, total := pullBytes(fields[2], fields[3]), pullBytes(fields[4], fields[5])
		if current > total || total == 0 {
			continue
		}
		layers[fields[1]] = current
		var measured int64
		for _, count := range layers {
			measured += count
		}
		// A layer total is not the full image total; publishing it as such would fake a percentage.
		report(DownloadProgress{DownloadedBytes: measured})
	}
	// Drain on malformed/oversized lines so a child cannot deadlock on a full output pipe.
	if scanner.Err() != nil {
		_, _ = io.Copy(io.Discard, reader)
	}
}

// RunProgress runs the same fixed command group as Run, parsing both output streams without logging them.
// report runs synchronously with output consumption; errors never expose command output.
func (ExecRunner) RunProgress(ctx context.Context, name string, args, environment []string, report func(DownloadProgress)) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	reader, writer := io.Pipe()
	command.Stdout = writer
	command.Stderr = writer
	done := make(chan struct{})
	go func() { defer close(done); scanPullProgress(reader, report); _ = reader.Close() }()
	err := command.Run()
	_ = writer.Close()
	<-done
	if err != nil {
		return errors.New("host command failed")
	}
	return nil
}
