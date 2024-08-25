//go:build linux
// +build linux

package gonotify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var TimeoutError = errors.New("Inotify timeout")

type getWatchRequest struct {
	pathName string
	result   chan uint32
}

type getPathRequest struct {
	wd     uint32
	result chan string
}

type addWatchRequest struct {
	pathName string
	wd       uint32
}

// Inotify is the low level wrapper around inotify_init(), inotify_add_watch() and inotify_rm_watch()
type Inotify struct {
	// ctx is the context of inotify instance
	ctx context.Context
	// fd is the file descriptor of inotify instance
	fd int

	// getWatchByPathIn is the channel for getting watch descriptor by path
	getWatchByPathIn chan getWatchRequest
	// getPathByWatchIn is the channel for getting path by watch descriptor
	getPathByWatchIn chan getPathRequest
	// addWatchIn is the channel for adding watch
	addWatchIn chan addWatchRequest
	// rmByWdIn is the channel for removing watch by watch descriptor
	rmByWdIn chan uint32
	// rmByPathIn is the channel for removing watch by path
	rmByPathIn chan string
}

// NewInotify creates new inotify instance
func NewInotify(ctx context.Context) (*Inotify, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}

	inotify := &Inotify{
		ctx:              ctx,
		fd:               fd,
		getPathByWatchIn: make(chan getPathRequest),
		getWatchByPathIn: make(chan getWatchRequest),
		addWatchIn:       make(chan addWatchRequest),
		rmByWdIn:         make(chan uint32),
		rmByPathIn:       make(chan string),
	}

	go func() {
		watches := make(map[string]uint32)
		paths := make(map[uint32]string)

		for {
			select {
			case <-ctx.Done():
				for _, w := range watches {
					_, err := syscall.InotifyRmWatch(fd, w)
					if err != nil {
						continue
					}
				}
				syscall.Close(fd)
				return
			case req := <-inotify.addWatchIn:
				watches[req.pathName] = req.wd
				paths[req.wd] = req.pathName
			case req := <-inotify.getWatchByPathIn:
				wd, ok := watches[req.pathName]
				if ok {
					req.result <- wd
				}
				close(req.result)
			case req := <-inotify.getPathByWatchIn:
				pathName, ok := paths[req.wd]
				if ok {
					req.result <- pathName
				}
				close(req.result)
			case wd := <-inotify.rmByWdIn:
				pathName, ok := paths[wd]
				if !ok {
					continue
				}
				delete(watches, pathName)
				delete(paths, wd)
			case pathName := <-inotify.rmByPathIn:
				wd, ok := watches[pathName]
				if !ok {
					continue
				}
				delete(watches, pathName)
				delete(paths, wd)
			}
		}
	}()

	return inotify, nil
}

// AddWatch adds given path to list of watched files / folders
func (i *Inotify) AddWatch(pathName string, mask uint32) error {
	w, err := syscall.InotifyAddWatch(i.fd, pathName, mask)
	if err != nil {
		return err
	}

	select {
	case <-i.ctx.Done():
		return i.ctx.Err()
	case i.addWatchIn <- addWatchRequest{
		pathName: pathName,
		wd:       uint32(w),
	}:
		return nil
	}
}

// RmWd removes watch by watch descriptor
func (i *Inotify) RmWd(wd uint32) error {
	select {
	case <-i.ctx.Done():
		return i.ctx.Err()
	case i.rmByWdIn <- wd:
		return nil
	}
}

// RmWatch removes watch by pathName
func (i *Inotify) RmWatch(pathName string) error {
	select {
	case <-i.ctx.Done():
		return i.ctx.Err()
	case i.rmByPathIn <- pathName:
		return nil
	}
}

// Read reads portion of InotifyEvents and may fail with an error. If no events are available, it will
// wait forever, until context is cancelled.
func (i *Inotify) Read() ([]InotifyEvent, error) {
	for {
		evts, err := i.ReadDeadline(time.Now().Add(time.Millisecond * 200))
		if err != nil {
			if err == TimeoutError {
				continue
			}
			return evts, err
		}
		if len(evts) > 0 {
			return evts, nil
		}
	}
}

// ReadDeadline waits for InotifyEvents until deadline is reached, or context is cancelled. If
// deadline is reached, TimeoutError is returned.
func (i *Inotify) ReadDeadline(deadline time.Time) ([]InotifyEvent, error) {
	events := make([]InotifyEvent, 0, 1024)
	buf := make([]byte, 1024*(syscall.SizeofInotifyEvent+16))

	var n int
	var err error

	fdset := &syscall.FdSet{}
	fdset.Bits[0] = 1 << uint(i.fd)

	for {
		if i.ctx.Err() != nil {
			return events, i.ctx.Err()
		}

		now := time.Now()

		if now.After(deadline) {
			return events, TimeoutError
		}

		diff := deadline.Sub(now)

		timeout := syscall.NsecToTimeval(diff.Nanoseconds())

		_, err = syscall.Select(i.fd+1, fdset, nil, nil, &timeout)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return events, err
		}

		n, err = syscall.Read(i.fd, buf)
		if err != nil {
			if err == syscall.EAGAIN {
				continue
			}
			return events, err
		}

		if n > 0 {
			break
		}
	}

	if n < syscall.SizeofInotifyEvent {
		return events, fmt.Errorf("Short inotify read")
	}

	offset := 0

	for offset+syscall.SizeofInotifyEvent <= n {

		event := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		namebuf := buf[offset+syscall.SizeofInotifyEvent : offset+syscall.SizeofInotifyEvent+int(event.Len)]

		offset += syscall.SizeofInotifyEvent + int(event.Len)

		name := strings.TrimRight(string(namebuf), "\x00")

		req := getPathRequest{
			wd:     uint32(event.Wd),
			result: make(chan string),
		}

		select {
		case <-i.ctx.Done():
			return events, i.ctx.Err()
		case i.getPathByWatchIn <- req:

			select {
			case <-i.ctx.Done():
				return events, i.ctx.Err()
			case watchName := <-req.result:
				name = filepath.Join(watchName, name)
			}
		}

		events = append(events, InotifyEvent{
			Wd:     uint32(event.Wd),
			Name:   name,
			Mask:   event.Mask,
			Cookie: event.Cookie,
		})
	}

	return events, nil
}
