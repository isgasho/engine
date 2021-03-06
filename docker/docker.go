package docker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	gosignal "os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/term"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"gopkg.in/src-d/go-log.v1"
)

type Port = types.Port

// GetClient returns a docker client if all checks pass.
// This function performs three checks:
//   1. checks that docker is installed and running properly,
//   2. checks that the user is not running docker toolbox.
//   3. checks that the client api version is supported by the docker engine,
func GetClient() (*client.Client, error) {
	log.Debugf("Creating docker client from env")
	// This will fail in case of bad response from the daemon or in
	// case of docker not installed/running
	c, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, err
	}

	log.Debugf("Checking for Docker Toolbox")
	var info types.Info
	// Get information from running daemon to check whether is running
	// docker toolbox
	info, err = c.Info(context.Background())
	if err != nil {
		return nil, err
	}

	if strings.Contains(strings.ToLower(info.OperatingSystem), "boot2docker") {
		return nil, fmt.Errorf("Docker Toolbox is not supported")
	}

	log.Debugf("Retrieving docker server version")
	// Call `ServerVersion` to force checking API version compatibility
	if _, err = c.ServerVersion(context.Background()); err != nil {
		return nil, err
	}

	return c, nil
}

func Version() (string, error) {
	c, err := GetClient()
	if err != nil {
		return "", errors.Wrap(err, "could not create docker client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ping, err := c.Ping(ctx)
	if err != nil {
		return "", errors.Wrap(err, "could not ping docker")
	}

	return ping.APIVersion, nil
}

var ErrNotFound = errors.New("container not found")

type Container = types.Container

func Info(name string) (*Container, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	filter := filters.NewArgs()
	filter.Add("name", name)

	cs, err := c.ContainerList(ctx, types.ContainerListOptions{
		All:     true,
		Filters: filter,
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not list containers")
	}

	for _, c := range cs {
		for _, n := range c.Names {
			if name == n[1:] {
				return &c, nil
			}
		}
	}
	return nil, ErrNotFound
}

func List() ([]Container, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	return c.ContainerList(context.Background(), types.ContainerListOptions{All: true})
}

// IsRunning returns true if the container with the given name is running. If
// image is not an empty string, it will return true only if the container
// image matches it (in the format imageName:version)
func IsRunning(name string, image string) (bool, error) {
	info, err := Info(name)
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// apparently there is no constant for "running" in the API, we use the
	// string value from the documentation
	if info.State != "running" {
		return false, nil
	}

	if image == "" {
		return true, nil
	}

	infoImgName, infoImgV := SplitImageID(info.Image)
	if infoImgV == "" {
		infoImgV = "latest"
	}

	imgName, imgV := SplitImageID(image)
	if imgV == "" {
		imgV = "latest"
	}

	return (imgName == infoImgName && imgV == infoImgV), nil
}

// RemoveContainer finds a container by name and force-remove it with timeout.
// It will also remove any anonymous volumes
func RemoveContainer(name string) error {
	info, err := Info(name)
	if err != nil {
		return err
	}

	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	return c.ContainerRemove(ctx, info.ID, types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

// IsInstalled checks whether an image is installed or not. If version is
// empty, it will check that any version is installed, otherwise it will check
// that the given version is installed.
func IsInstalled(ctx context.Context, image, version string) (bool, error) {
	versions, err := VersionsInstalled(ctx, image)
	if err != nil {
		return false, err
	}

	if version == "" {
		return len(versions) > 0, nil
	}

	for _, v := range versions {
		if v == version {
			return true, nil
		}
	}

	return false, nil
}

// VersionsInstalled returns a list of versions installed for the given image
// name
func VersionsInstalled(ctx context.Context, image string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	imgs, err := ListImages(ctx)
	if err != nil {
		return nil, err
	}

	res := make([]string, 0)

	for _, i := range imgs {
		for _, repoTag := range i.RepoTags {
			img, v := SplitImageID(repoTag)
			if image == img {
				res = append(res, v)
			}
		}
	}

	return res, nil
}

// SplitImageID splits an image ID (imageName:version) into image name and version
func SplitImageID(id string) (image, version string) {
	parts := strings.Split(id, ":")
	image = parts[0]
	version = "latest"
	if len(parts) > 1 {
		version = parts[1]
	}
	return
}

// Pull an image from docker hub with a specific version.
func Pull(ctx context.Context, image, version string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	id := image + ":" + version
	rc, err := c.ImagePull(ctx, id, types.ImagePullOptions{})
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("could not pull image %q", id))
	}

	io.Copy(ioutil.Discard, rc)

	return rc.Close()
}

// EnsureInstalled checks whether an image is installed or not. If version is
// empty, it will check that any version is installed, otherwise it will check
// that the given version is installed. If the image is not installed, it will
// be automatically installed.
func EnsureInstalled(image, version string) error {
	ok, err := IsInstalled(context.Background(), image, version)
	if err != nil {
		return err
	}

	if ok {
		return nil
	}

	if version == "" {
		version = "latest"
	}
	id := image + ":" + version

	log.Infof("installing %q", id)

	if err := Pull(context.Background(), image, version); err != nil {
		return err
	}

	log.Infof("installed %q", id)

	return nil
}

// HostPath returns the correct host path to use depending on the host OS
func HostPath(hostPath string) (string, error) {
	c, err := GetClient()
	if err != nil {
		return "", errors.Wrap(err, "could not create docker client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	info, err := c.Info(ctx)
	if err != nil {
		return "", errors.Wrap(err, "could not get information about docker server")
	}

	isWinHost := info.OSType == "windows" || strings.Contains(
		strings.ToLower(info.OperatingSystem), "windows")
	if isWinHost {
		// For Windows we need to change paths like
		// C:/Users/Windows10/go/src/github.com/src-d/engine to
		// //c/Users/Windows10/go/src/github.com/src-d/engine
		hostPath = regexp.MustCompile(`^(\w):`).ReplaceAllStringFunc(hostPath, func(m string) string {
			return "//" + strings.ToLower(m[:len(m)-1])
		})
	}

	return hostPath, nil
}

type ConfigOption func(*container.Config, *container.HostConfig)

func WithEnv(key, value string) ConfigOption {
	return func(cfg *container.Config, hc *container.HostConfig) {
		cfg.Env = append(cfg.Env, key+"="+value)
	}
}

func WithVolume(name, containerPath, hostOS string) ConfigOption {
	return withVolume(mount.TypeVolume, name, containerPath, false, hostOS)
}

func WithSharedDirectory(hostPath, containerPath, hostOS string) ConfigOption {
	return withVolume(mount.TypeBind, hostPath, containerPath, false, hostOS)
}

func WithROSharedDirectory(hostPath, containerPath, hostOS string) ConfigOption {
	return withVolume(mount.TypeBind, hostPath, containerPath, true, hostOS)
}

func withVolume(typ mount.Type, hostPath, containerPath string, readOnly bool, hostOS string) ConfigOption {
	return func(cfg *container.Config, hc *container.HostConfig) {
		m := mount.Mount{
			Type:     typ,
			Source:   hostPath,
			Target:   containerPath,
			ReadOnly: readOnly,
		}
		if hostOS != "" && hostOS != "linux" {
			m.Consistency = mount.ConsistencyDelegated
		}
		hc.Mounts = append(hc.Mounts, m)
	}
}

// WithPort adds a port binding. If publicPort is 0 it means the port will be
// chosen by docker, if it is -1 it will be the same one as privatePort
func WithPort(publicPort, privatePort int) ConfigOption {
	return func(cfg *container.Config, hc *container.HostConfig) {
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = make(nat.PortSet)
		}

		if hc.PortBindings == nil {
			hc.PortBindings = make(nat.PortMap)
		}

		port := nat.Port(fmt.Sprint(privatePort))
		cfg.ExposedPorts[port] = struct{}{}
		hc.PortBindings[port] = append(
			hc.PortBindings[port],
			nat.PortBinding{HostPort: fmt.Sprint(publicPort)},
		)
	}
}

// WithCmd appends arguments to the cmd arguments.
func WithCmd(args ...string) ConfigOption {
	return func(cfg *container.Config, hc *container.HostConfig) {
		cfg.Cmd = append(cfg.Cmd, args...)
	}
}

func ApplyOptions(c *container.Config, hc *container.HostConfig, opts ...ConfigOption) {
	for _, o := range opts {
		o(c, hc)
	}
}

type StartFunc func(ctx context.Context) error

func InfoOrStart(ctx context.Context, name string, start StartFunc) (*Container, error) {
	running, err := IsRunning(name, "")
	if err != nil {
		return nil, err
	}

	if !running {
		if err := start(ctx); err != nil {
			return nil, errors.Wrapf(err, "could not create %s", name)
		}
	}

	return Info(name)
}

// Start creates, starts and connect new container to src-d network
// if container already exists but stopped it removes it first to make sure it has correct configuration
func Start(ctx context.Context, config *container.Config, host *container.HostConfig, name string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	res, err := forceContainerCreate(ctx, c, config, host, name)
	if err != nil {
		return errors.Wrapf(err, "could not create container %s", name)
	}

	if err := c.ContainerStart(ctx, res.ID, types.ContainerStartOptions{}); err != nil {
		return errors.Wrapf(err, "could not start container: %s", name)
	}

	// TODO: remove this hack
	time.Sleep(time.Second)

	err = connectToNetwork(ctx, res.ID)
	return errors.Wrapf(err, "could not connect to network")
}

// forceContainerCreate tries to create container
// in case of error it deletes container and tries again
func forceContainerCreate(
	ctx context.Context,
	c *client.Client,
	config *container.Config,
	host *container.HostConfig,
	name string,
) (container.ContainerCreateCreatedBody, error) {
	res, err := c.ContainerCreate(ctx, config, host, &network.NetworkingConfig{}, name)
	if err == nil {
		return res, nil
	}

	// in case of error res doesn't contain ID of the container
	info, errInfo := Info(name)
	if errInfo != nil {
		return res, err
	}

	err = c.ContainerRemove(ctx, info.ID, types.ContainerRemoveOptions{Force: true})
	if err != nil {
		log.Errorf(err, "could not remove container after failing to create it")
		return res, err
	}

	res, err = c.ContainerCreate(ctx, config, host, &network.NetworkingConfig{}, name)
	if err == nil {
		return res, nil
	}

	return res, err
}

func CreateVolume(ctx context.Context, name string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	_, err = c.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}

	_, err = c.VolumeCreate(ctx, volume.VolumeCreateBody{Name: name})
	return err
}

type Volume = types.Volume

func ListVolumes(ctx context.Context) ([]*Volume, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	list, err := c.VolumeList(ctx, filters.Args{})
	if err != nil {
		return nil, errors.Wrap(err, "could not get list of volumes")
	}

	return list.Volumes, nil
}

type Image = types.ImageSummary

func ListImages(ctx context.Context) ([]Image, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	images, err := c.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "could not get list of images")
	}

	return images, nil
}

type Network = types.NetworkResource

func ListNetworks(ctx context.Context) ([]Network, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	networks, err := c.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "could not get list of networks")
	}

	return networks, nil
}

func RemoveVolume(ctx context.Context, id string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	return c.VolumeRemove(ctx, id, true)
}

func RemoveImage(ctx context.Context, id string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	_, err = c.ImageRemove(ctx, id, types.ImageRemoveOptions{Force: true})
	return err
}

// NetworkName is the name of the srcd docker network
const NetworkName = "srcd-cli-network"

func connectToNetwork(ctx context.Context, containerID string) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	if _, err := c.NetworkInspect(ctx, NetworkName, types.NetworkInspectOptions{}); err != nil {
		log.Debugf("couldn't find network %s: %v", NetworkName, err)
		log.Infof("creating %s docker network", NetworkName)
		_, err = c.NetworkCreate(ctx, NetworkName, types.NetworkCreate{})
		if err != nil {
			return errors.Wrap(err, "could not create network")
		}
	}
	return c.NetworkConnect(ctx, NetworkName, containerID, nil)
}

func RemoveNetwork(ctx context.Context) error {
	c, err := GetClient()
	if err != nil {
		return errors.Wrap(err, "could not create docker client")
	}

	resp, err := c.NetworkInspect(ctx, NetworkName, types.NetworkInspectOptions{})
	if client.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "could not inspect network")
	}

	return c.NetworkRemove(ctx, resp.ID)
}

func GetLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	c, err := GetClient()
	if err != nil {
		return nil, errors.Wrap(err, "could not create docker client")
	}

	reader, err := c.ContainerLogs(ctx, containerID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Since:      time.Now().Format(time.RFC3339Nano),
	})

	return reader, err
}

// Attach works similar to docker run -it
// it creates container, attaches to the input & output and then starts container
// it returns connection to read/write into the container and channel with exit code
func Attach(ctx context.Context, config *container.Config, host *container.HostConfig, name string) (*types.HijackedResponse, chan int64, error) {
	c, err := GetClient()
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not create docker client")
	}

	// update config with attach options
	config.AttachStdin = true
	config.AttachStdout = true
	config.AttachStderr = true
	config.OpenStdin = true
	config.Tty = true

	// Telling the Windows daemon the initial size of the tty during start makes
	// a far better user experience rather than relying on subsequent resizes
	// to cause things to catch up.
	// https://github.com/docker/docker-ce/blob/eb973f58a00c48bcde97f61a7903b8d474f6c6c0/components/cli/cli/command/container/run.go#L123
	if runtime.GOOS == "windows" {
		host.ConsoleSize[0], host.ConsoleSize[1] = getStdOutSize()
	}

	res, err := forceContainerCreate(ctx, c, config, host, name)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not create container %s", name)
	}

	err = connectToNetwork(ctx, res.ID)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not connect to network")
	}

	resp, err := c.ContainerAttach(ctx, res.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not attach to container")
	}

	if err := c.ContainerStart(ctx, res.ID, types.ContainerStartOptions{}); err != nil {
		return nil, nil, errors.Wrapf(err, "could not start container: %s", name)
	}

	exit := make(chan int64, 1)
	go func() {
		var code int64
		waitBody, errCh := c.ContainerWait(ctx, res.ID, container.WaitConditionNotRunning)
		select {
		case <-errCh:
			code = 1
		case body := <-waitBody:
			code = body.StatusCode
		}
		exit <- code
	}()

	monitorTtySize(c, res.ID)

	return &resp, exit, nil
}

func getStdOutSize() (uint, uint) {
	fd, isTerminal := term.GetFdInfo(os.Stdout)
	if !isTerminal {
		return 0, 0
	}

	ws, err := term.GetWinsize(fd)
	if err != nil {
		return 0, 0
	}

	return uint(ws.Height), uint(ws.Width)
}

func monitorTtySize(c *client.Client, containerID string) {
	initTtySize(c, containerID)
	if runtime.GOOS == "windows" {
		go func() {
			prevH, prevW := getStdOutSize()
			for {
				time.Sleep(time.Millisecond * 250)
				h, w := getStdOutSize()

				if prevW != w || prevH != h {
					resizeTty(c, containerID)
				}
				prevH = h
				prevW = w
			}
		}()
	} else {
		sigchan := make(chan os.Signal, 1)
		gosignal.Notify(sigchan, signal.SIGWINCH)
		go func() {
			for range sigchan {
				resizeTty(c, containerID)
			}
		}()
	}
}

// initTtySize is to init the tty's size to the same as the window, if there is an error, it will retry 5 times.
func initTtySize(c *client.Client, containerID string) {
	if err := resizeTty(c, containerID); err != nil {
		go func() {
			var err error
			for retry := 0; retry < 5; retry++ {
				time.Sleep(10 * time.Millisecond)
				if err = resizeTty(c, containerID); err == nil {
					break
				}
			}
		}()
	}
}

func resizeTty(c *client.Client, containerID string) error {
	height, width := getStdOutSize()
	return c.ContainerResize(context.TODO(), containerID, types.ResizeOptions{
		Height: height,
		Width:  width,
	})
}
