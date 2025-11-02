package main

/*
#cgo CFLAGS: -DWIN32_LEAN_AND_MEAN
#cgo windows LDFLAGS: -lws2_32 -lwinmm -lntdll -luser32 -lkernel32 -lcrypt32 -liphlpapi
#include <stdint.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	_ "github.com/xtls/xray-core/app/dispatcher"
	_ "github.com/xtls/xray-core/app/policy"

	_ "github.com/xtls/xray-core/app/proxyman"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"

	core "github.com/xtls/xray-core/core"
	serial "github.com/xtls/xray-core/infra/conf/serial"

	_ "github.com/xtls/xray-core/proxy/freedom"
	_ "github.com/xtls/xray-core/proxy/http"
	_ "github.com/xtls/xray-core/proxy/socks"

	_ "github.com/xtls/xray-core/transport/internet/tcp"
	_ "github.com/xtls/xray-core/transport/internet/udp"

	_ "github.com/xtls/xray-core/proxy/vless"
	_ "github.com/xtls/xray-core/transport/internet/grpc"
	_ "github.com/xtls/xray-core/transport/internet/quic"
	_ "github.com/xtls/xray-core/transport/internet/tls"
	_ "github.com/xtls/xray-core/transport/internet/websocket"
)

// Global state (single embedded instance for simplicity). Adjust if you need multi-instance.
var (
	instMu   sync.Mutex
	instance *core.Instance
	lastErr  string
	logBufMu sync.Mutex
	logBuf   bytes.Buffer
)

func setErr(err error) C.int {
	if err == nil {
		lastErr = ""
		return 0
	}
	lastErr = err.Error()
	return 1
}

// writeLog is a very small logger sink you can poll from C.
func writeLog(msg string) {
	logBufMu.Lock()
	defer logBufMu.Unlock()
	logBuf.WriteString(time.Now().Format(time.RFC3339))
	logBuf.WriteString(" ")
	logBuf.WriteString(msg)
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		logBuf.WriteByte('\n')
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Utilities to create and run Xray instance
// ──────────────────────────────────────────────────────────────────────────────

// buildInstance builds a core.Instance from JSON config bytes.
func buildInstance(jsonCfg []byte) (*core.Instance, error) {
	r := bytes.NewReader(jsonCfg)

	typedCfg, err := serial.DecodeJSONConfig(r)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	coreCfg, err := typedCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build core config: %w", err)
	}

	ins, err := core.New(coreCfg)
	if err != nil {
		return nil, fmt.Errorf("core.New: %w", err)
	}
	return ins, nil
}

// startInstance starts the instance and installs a basic logger via environment.
func startInstance(ins *core.Instance) error {
	if err := ins.Start(); err != nil {
		return err
	}
	writeLog("xray: started")
	return nil
}

// stopInstance stops and closes the instance.
func stopInstance() error {
	if instance == nil {
		return nil
	}
	err := instance.Close()
	instance = nil
	if err != nil {
		return err
	}
	writeLog("xray: stopped")
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// C ABI (exported)
// ──────────────────────────────────────────────────────────────────────────────

//export Xray_Version
func Xray_Version() *C.char {
	return C.CString(core.Version())
}

//export Xray_LastError
func Xray_LastError(buf *C.char, buflen C.uint32_t) C.uint32_t {
	// Copy lastErr into caller-provided buffer (UTF-8, NUL-terminated)
	s := lastErr
	if len(s) == 0 {
		s = ""
	}
	b := append([]byte(s), 0)
	if int(buflen) <= 0 || buf == nil {
		return 0
	}
	max := int(buflen) - 1
	if max < 0 {
		max = 0
	}
	if len(b)-1 > max {
		b = append([]byte(s[:max]), 0)
	}
	//nolint:gosec
	for i := 0; i < len(b); i++ {
		*(*byte)(unsafe.Add(unsafe.Pointer(buf), i)) = b[i]
	}
	return C.uint32_t(len(b) - 1)
}

//export Xray_Start
func Xray_Start(jsonCfg *C.char) C.int {
	instMu.Lock()
	defer instMu.Unlock()
	if instance != nil {
		return setErr(errors.New("already started"))
	}
	if jsonCfg == nil {
		return setErr(errors.New("nil config"))
	}
	cfgBytes := []byte(C.GoString(jsonCfg))
	ins, err := buildInstance(cfgBytes)
	if err != nil {
		return setErr(err)
	}
	if err := startInstance(ins); err != nil {
		return setErr(err)
	}
	instance = ins
	return setErr(nil)
}

//export Xray_Stop
func Xray_Stop() C.int {
	instMu.Lock()
	defer instMu.Unlock()
	return setErr(stopInstance())
}

//export Xray_Reload
func Xray_Reload(jsonCfg *C.char) C.int {
	instMu.Lock()
	defer instMu.Unlock()
	if instance == nil {
		return setErr(errors.New("not running"))
	}
	if jsonCfg == nil {
		return setErr(errors.New("nil config"))
	}
	cfgBytes := []byte(C.GoString(jsonCfg))
	newIns, err := buildInstance(cfgBytes)
	if err != nil {
		return setErr(err)
	}
	// Start new first, then stop old for minimal downtime.
	if err := startInstance(newIns); err != nil {
		return setErr(err)
	}
	_ = instance.Close()
	instance = newIns
	writeLog("xray: reloaded")
	return setErr(nil)
}

//export Xray_PingOutbound
// func Xray_PingOutbound(tag *C.char, host *C.char, port C.uint16_t, timeoutMs C.uint32_t) C.int {
// 	if instance == nil {
// 		return setErr(errors.New("not running"))
// 	}
// 	dialer := instance.Outbound(tagOrEmpty(tag))
// 	if dialer == nil {
// 		return setErr(fmt.Errorf("outbound not found: %s", C.GoString(tag)))
// 	}
// 	addr := fmt.Sprintf("%s:%d", C.GoString(host), uint16(port))
// 	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
// 	defer cancel()
// 	c, err := dialer.Dial(ctx, net.Network_TCP, addr)
// 	if err != nil {
// 		return setErr(err)
// 	}
// 	_ = c.Close()
// 	return setErr(nil)
// }

func tagOrEmpty(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

//export Xray_PollLog
func Xray_PollLog(dst *C.char, dstlen C.uint32_t) C.uint32_t {
	if dst == nil || dstlen == 0 {
		return 0
	}
	logBufMu.Lock()
	defer logBufMu.Unlock()
	if logBuf.Len() == 0 {
		*(*byte)(unsafe.Pointer(dst)) = 0
		return 0
	}
	// Return up to dstlen-1 bytes
	n := int(dstlen) - 1
	if n < 0 {
		n = 0
	}
	b := logBuf.Next(n)
	b = append(b, 0)
	for i := 0; i < len(b); i++ {
		*(*byte)(unsafe.Add(unsafe.Pointer(dst), i)) = b[i]
	}
	return C.uint32_t(len(b) - 1)
}

//export Xray_StatsQuery
func Xray_StatsQuery(name *C.char) C.int {
	if instance == nil {
		return setErr(errors.New("not running"))
	}
	// n := C.GoString(name)
	// ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	// defer cancel()
	// if _, err := instance.Stats(ctx, n); err != nil {
	// 	return setErr(err)
	// }
	return setErr(nil)
}

//export Xray_WriteConfigFile
func Xray_WriteConfigFile(path *C.char, jsonCfg *C.char) C.int {
	if path == nil || jsonCfg == nil {
		return setErr(errors.New("nil param"))
	}
	if err := os.WriteFile(C.GoString(path), []byte(C.GoString(jsonCfg)), 0600); err != nil {
		return setErr(err)
	}
	return setErr(nil)
}

//export Xray_ReadFile
func Xray_ReadFile(path *C.char, dst *C.char, dstlen C.uint32_t) C.uint32_t {
	if path == nil || dst == nil || dstlen == 0 {
		return 0
	}
	f, err := os.Open(C.GoString(path))
	if err != nil {
		setErr(err)
		return 0
	}
	defer f.Close()
	buf := make([]byte, int(dstlen)-1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		setErr(err)
		return 0
	}
	// NUL terminate
	buf = append(buf[:n], 0)
	for i := 0; i < len(buf); i++ {
		*(*byte)(unsafe.Add(unsafe.Pointer(dst), i)) = buf[i]
	}
	return C.uint32_t(n)
}

// ──────────────────────────────────────────────────────────────────────────────
// Required to satisfy cshared main
// ──────────────────────────────────────────────────────────────────────────────
func main() {}
