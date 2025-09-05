package gadb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type DeviceFileInfo struct {
	Name         string
	Mode         os.FileMode
	Size         uint32
	LastModified time.Time
}

func (info DeviceFileInfo) IsDir() bool {
	return (info.Mode & (1 << 14)) == (1 << 14)
}

const DefaultFileMode = os.FileMode(0664)

type DeviceState string

const (
	StateUnknown      DeviceState = "UNKNOWN"
	StateOnline       DeviceState = "online"
	StateOffline      DeviceState = "offline"
	StateDisconnected DeviceState = "disconnected"
)

var deviceStateStrings = map[string]DeviceState{
	"":        StateDisconnected,
	"offline": StateOffline,
	"device":  StateOnline,
}

func deviceStateConv(k string) (deviceState DeviceState) {
	var ok bool
	if deviceState, ok = deviceStateStrings[k]; !ok {
		return StateUnknown
	}
	return
}

type Port = string

type DeviceForward struct {
	Serial string
	Local  string
	Remote string
	// LocalProtocol string
	// RemoteProtocol string
}

// TcpPort builds a tcp:<port> endpoint string for Forward/Reverse.
func TcpPort(port int) Port { return Port(fmt.Sprintf("tcp:%d", port)) }

// LocalAbstractPort builds a localabstract:<path> endpoint string for Android's abstract UNIX domain socket namespace.
func LocalAbstractPort(path string) Port { return Port(fmt.Sprintf("localabstract:%s", path)) }

type Device struct {
	adbClient Client
	serial    string
	attrs     map[string]string
}

func (d Device) HasAttribute(key string) bool {
	_, ok := d.attrs[key]
	return ok
}

func (d Device) Product() (string, error) {
	if d.HasAttribute("product") {
		return d.attrs["product"], nil
	}
	return "", errors.New("does not have attribute: product")
}

func (d Device) Model() (string, error) {
	if d.HasAttribute("model") {
		return d.attrs["model"], nil
	}
	return "", errors.New("does not have attribute: model")
}

func (d Device) Usb() (string, error) {
	if d.HasAttribute("usb") {
		return d.attrs["usb"], nil
	}
	return "", errors.New("does not have attribute: usb")
}

func (d Device) transportId() (string, error) {
	if d.HasAttribute("transport_id") {
		return d.attrs["transport_id"], nil
	}
	return "", errors.New("does not have attribute: transport_id")
}

func (d Device) DeviceInfo() map[string]string {
	return d.attrs
}

func (d Device) Serial() string {
	// 	resp, err := d.adbClient.executeCommand(fmt.Sprintf("host-serial:%s:get-serialno", d.serial))
	return d.serial
}

func (d Device) IsUsb() (bool, error) {
	usb, err := d.Usb()
	if err != nil {
		return false, err
	}

	return usb != "", nil
}

func (d Device) State() (DeviceState, error) {
	resp, err := d.adbClient.executeCommand(fmt.Sprintf("host-serial:%s:get-state", d.serial))
	return deviceStateConv(resp), err
}

func (d Device) DevicePath() (string, error) {
	resp, err := d.adbClient.executeCommand(fmt.Sprintf("host-serial:%s:get-devpath", d.serial))
	return resp, err
}

func (d Device) Forward(local, remote Port, noRebind ...bool) (err error) {
	command := ""
	if len(noRebind) != 0 && noRebind[0] {
		command = fmt.Sprintf("host-serial:%s:forward:norebind:%s;%s", d.serial, local, remote)
	} else {
		command = fmt.Sprintf("host-serial:%s:forward:%s;%s", d.serial, local, remote)
	}
	_, err = d.adbClient.executeCommand(command, true)
	return
}

func (d Device) ForwardList() (deviceForwardList []DeviceForward, err error) {
	var forwardList []DeviceForward
	if forwardList, err = d.adbClient.ForwardList(); err != nil {
		return nil, err
	}

	deviceForwardList = make([]DeviceForward, 0, len(deviceForwardList))
	for i := range forwardList {
		if forwardList[i].Serial == d.serial {
			deviceForwardList = append(deviceForwardList, forwardList[i])
		}
	}
	// resp, err := d.adbClient.executeCommand(fmt.Sprintf("host-serial:%s:list-forward", d.serial))
	return
}

func (d Device) ForwardKill(local Port) (err error) {
	_, err = d.adbClient.executeCommand(fmt.Sprintf("host-serial:%s:killforward:%s", d.serial, local), true)
	return
}

func (d Device) RunShellCommand(cmd string, args ...string) (string, error) {
	raw, err := d.RunShellCommandWithBytes(cmd, args...)
	return string(raw), err
}

func (d Device) RunShellCommandWithBytes(cmd string, args ...string) ([]byte, error) {
	if len(args) > 0 {
		cmd = fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	}
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("adb shell: command cannot be empty")
	}
	raw, err := d.executeCommand(fmt.Sprintf("shell:%s", cmd))
	return raw, err
}

// RunShellCommandAsync starts a long-running shell command on the device and returns
// a Shell handle that can be used to stream output and forcefully stop the command
// via Shell.Close(). The returned Shell.Reader streams combined stdout/stderr.
func (d Device) RunShellCommandAsync(cmd string, args ...string) (*Shell, error) {
	if len(args) > 0 {
		cmd = fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	}
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("adb shell: command cannot be empty")
	}

	// Establish device transport
	tp, err := d.createDeviceTransport()
	if err != nil {
		return nil, err
	}
	// We intentionally do NOT defer tp.Close() here because we return a live Shell.

	// Use shell v2 protocol by sending the "shell:<cmd>" service and then wrapping
	// the underlying connection with shellTransport to read multiplexed streams.
	if err = tp.Send(fmt.Sprintf("shell:%s", cmd)); err != nil {
		_ = tp.Close()
		return nil, err
	}
	if err = tp.VerifyResponse(); err != nil {
		_ = tp.Close()
		return nil, err
	}

	shTp, err := tp.CreateShellTransport()
	if err != nil {
		_ = tp.Close()
		return nil, err
	}

	shell := &Shell{st: shTp}
	shell.Reader = newShellReader(&shell.st)
	return shell, nil
}

func (d Device) EnableAdbOverTCP(port ...int) (err error) {
	if len(port) == 0 {
		port = []int{AdbDaemonPort}
	}

	_, err = d.executeCommand(fmt.Sprintf("tcpip:%d", port[0]), true)
	return
}

func (d Device) createDeviceTransport() (tp transport, err error) {
	if tp, err = newTransport(fmt.Sprintf("%s:%d", d.adbClient.host, d.adbClient.port)); err != nil {
		return transport{}, err
	}

	if err = tp.Send(fmt.Sprintf("host:transport:%s", d.serial)); err != nil {
		return transport{}, err
	}
	err = tp.VerifyResponse()
	return
}

func (d Device) executeCommand(command string, onlyVerifyResponse ...bool) (raw []byte, err error) {
	if len(onlyVerifyResponse) == 0 {
		onlyVerifyResponse = []bool{false}
	}

	var tp transport
	if tp, err = d.createDeviceTransport(); err != nil {
		return nil, err
	}
	defer func() { _ = tp.Close() }()

	if err = tp.Send(command); err != nil {
		return nil, err
	}

	if err = tp.VerifyResponse(); err != nil {
		return nil, err
	}

	if onlyVerifyResponse[0] {
		return
	}

	raw, err = tp.ReadBytesAll()
	return
}

func (d Device) List(remotePath string) (devFileInfos []DeviceFileInfo, err error) {
	var tp transport
	if tp, err = d.createDeviceTransport(); err != nil {
		return nil, err
	}
	defer func() { _ = tp.Close() }()

	var sync syncTransport
	if sync, err = tp.CreateSyncTransport(); err != nil {
		return nil, err
	}
	defer func() { _ = sync.Close() }()

	if err = sync.Send("LIST", remotePath); err != nil {
		return nil, err
	}

	devFileInfos = make([]DeviceFileInfo, 0)

	var entry DeviceFileInfo
	for entry, err = sync.ReadDirectoryEntry(); err == nil; entry, err = sync.ReadDirectoryEntry() {
		if entry == (DeviceFileInfo{}) {
			break
		}
		devFileInfos = append(devFileInfos, entry)
	}

	return
}

func (d Device) PushFile(local *os.File, remotePath string, modification ...time.Time) (err error) {
	if len(modification) == 0 {
		var stat os.FileInfo
		if stat, err = local.Stat(); err != nil {
			return err
		}
		modification = []time.Time{stat.ModTime()}
	}

	return d.Push(local, remotePath, modification[0], DefaultFileMode)
}

func (d Device) Push(source io.Reader, remotePath string, modification time.Time, mode ...os.FileMode) (err error) {
	if len(mode) == 0 {
		mode = []os.FileMode{DefaultFileMode}
	}

	var tp transport
	if tp, err = d.createDeviceTransport(); err != nil {
		return err
	}
	defer func() { _ = tp.Close() }()

	var sync syncTransport
	if sync, err = tp.CreateSyncTransport(); err != nil {
		return err
	}
	defer func() { _ = sync.Close() }()

	data := fmt.Sprintf("%s,%d", remotePath, mode[0])
	if err = sync.Send("SEND", data); err != nil {
		return err
	}

	if err = sync.SendStream(source); err != nil {
		return
	}

	if err = sync.SendStatus("DONE", uint32(modification.Unix())); err != nil {
		return
	}

	if err = sync.VerifyStatus(); err != nil {
		return
	}
	return
}

func (d Device) Pull(remotePath string, dest io.Writer) (err error) {
	var tp transport
	if tp, err = d.createDeviceTransport(); err != nil {
		return err
	}
	defer func() { _ = tp.Close() }()

	var sync syncTransport
	if sync, err = tp.CreateSyncTransport(); err != nil {
		return err
	}
	defer func() { _ = sync.Close() }()

	if err = sync.Send("RECV", remotePath); err != nil {
		return err
	}

	err = sync.WriteStream(dest)
	return
}

func (d Device) Logcat(dst io.Writer, exitChan chan bool) error {
	var tp transport
	var err error
	if tp, err = d.createDeviceTransport(); err != nil {
		return err
	}
	defer func() { _ = tp.Close() }()

	if err = tp.Send("shell:logcat"); err != nil {
		return err
	}
	if err = tp.VerifyResponse(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		r := NewReader(ctx, tp.sock)
		io.Copy(dst, r)
	}()
	<-exitChan
	cancel()
	return err
}

func (d Device) Logcat2File(file string, exitChan chan bool) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_SYNC|os.O_APPEND, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.Logcat(f, exitChan)
}

func (d Device) LogcatClear() error {
	_, err := d.executeCommand("shell:logcat -c")
	return err
}

// Reverse sets up reverse port forwarding from device to host.
func (d Device) Reverse(local, remote Port, noRebind ...bool) (err error) {
	command := ""
	if len(noRebind) != 0 && noRebind[0] {
		command = fmt.Sprintf("reverse:forward:norebind:%s;%s", local, remote)
	} else {
		command = fmt.Sprintf("reverse:forward:%s;%s", local, remote)
	}
	_, err = d.executeCommand(command, true)
	return
}

// ReverseList lists reverse forwards on the device. Serial is 'host' per adb spec.
func (d Device) ReverseList() (deviceForwardList []DeviceForward, err error) {
	raw, err := d.executeCommand("reverse:list-forward")
	if err != nil {
		return nil, err
	}
	resp := string(raw)
	lines := strings.Split(resp, "\n")
	deviceForwardList = make([]DeviceForward, 0, len(lines))
	for i := range lines {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			// Should be at least local and remote; adb usually provides 2 fields for reverse (serial is 'host' in docs),
			// but to keep consistent with ForwardList format, handle both 2 or 3 fields.
			continue
		}
		if len(fields) == 2 {
			deviceForwardList = append(deviceForwardList, DeviceForward{Serial: "host", Local: fields[0], Remote: fields[1]})
		} else {
			deviceForwardList = append(deviceForwardList, DeviceForward{Serial: fields[0], Local: fields[1], Remote: fields[2]})
		}
	}
	return
}

// ReverseKill removes a specific reverse forward from the device.
func (d Device) ReverseKill(local Port) (err error) {
	_, err = d.executeCommand(fmt.Sprintf("reverse:killforward:%s", local), true)
	return
}

// ReverseKillAll removes all reverse forwards on the device.
func (d Device) ReverseKillAll() (err error) {
	_, err = d.executeCommand("reverse:killforward-all", true)
	return
}
