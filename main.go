package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IceWhaleTech/CasaOS-Common/external"
	"github.com/IceWhaleTech/CasaOS-Common/model"
	"github.com/IceWhaleTech/CasaOS-Common/utils/constants"
	http2 "github.com/IceWhaleTech/CasaOS-Common/utils/http"
	"github.com/IceWhaleTech/CasaOS-Common/utils/logger"
	"github.com/coreos/go-systemd/daemon"

	"github.com/IceWhaleTech/CasaOS-Gateway/common"
	"github.com/IceWhaleTech/CasaOS-Gateway/route"
	"github.com/IceWhaleTech/CasaOS-Gateway/service"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	_state   *service.State
	_gateway *http.Server

	_managementServiceReady = make(chan struct{})
	_gatewayServiceReady    = make(chan struct{})

	ErrCheckURLNotOK = errors.New("check url did not return 200 OK")

	//go:embed build/sysroot/etc/casaos/gateway.ini.sample
	// [common]
	// runtimepath=/var/run/casaos
	// [gateway]
	// port=
	_confSample string
)

func init() {
	versionFlag := flag.Bool("v", false, "version")
	wwwPathFlag := flag.String("w", filepath.Join(constants.DefaultDataPath, "www"), "www path")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.Version)
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	_state = service.NewState()

	// create default config file if not exist
	// /etc/casaos/gateway.ini
	// [common]
	// runtimepath=/var/run/casaos

	// [gateway]
	// logfileext=log
	// logpath=/var/log/casaos
	// logsavename=gateway
	// port=80
	// wwwpath=/var/lib/casaos/www
	ConfigFilePath := filepath.Join(constants.DefaultConfigPath, common.GatewayName+"."+common.GatewayConfigType)
	// 判断文件是否存在，不存在则创建它
	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file not exist, create it")
		// 创建配置文件
		file, err := os.Create(ConfigFilePath)
		if err != nil {
			panic(err)
		}
		defer file.Close()

		// 写入默认配置
		_, err = file.WriteString(_confSample)
		if err != nil {
			panic(err)
		}
	}
	// 加载config文件
	config, err := common.LoadConfig()
	if err != nil {
		panic(err)
	}
	// 初始化 /var/log/casaos/gateway.log文件
	logger.LogInit(
		config.GetString(common.ConfigKeyLogPath),
		config.GetString(common.ConfigKeyLogSaveName),
		config.GetString(common.ConfigKeyLogFileExt),
	)
	// 获取运行路径 /var/run/casaos
	runtimePath := config.GetString(common.ConfigKeyRuntimePath)
	if err := _state.SetRuntimePath(runtimePath); err != nil {
		logger.Error("Failed to set runtime path", zap.Any("error", err), zap.Any(common.ConfigKeyRuntimePath, runtimePath))
		panic(err)
	}
	// 获取gateway端口号80
	gatewayPort := config.GetString(common.ConfigKeyGatewayPort)
	if err := _state.SetGatewayPort(gatewayPort); err != nil {
		logger.Error("Failed to set gateway port", zap.Any("error", err), zap.Any(common.ConfigKeyGatewayPort, gatewayPort))
		panic(err)
	}
	// 设置www path
	if err := _state.SetWWWPath(*wwwPathFlag); err != nil {
		logger.Error("Failed to set www path", zap.Any("error", err), zap.String("wwwpath", *wwwPathFlag))
		panic(err)
	}

	if err := checkPrequisites(_state); err != nil {
		logger.Error("Failed to check prequisites", zap.Any("error", err))
		panic(err)
	}

	_state.OnGatewayPortChange(func(s string) error {
		config.Set(common.ConfigKeyGatewayPort, _state.GetGatewayPort())
		config.Set(common.ConfigKeyRuntimePath, _state.GetRuntimePath())

		return config.WriteConfig()
	})
}

func main() {
	pidFilename, err := writePidFile(_state.GetRuntimePath())
	if err != nil {
		logger.Error("Failed to write pid file to runtime path", zap.Any("error", err), zap.Any("runtimePath", _state.GetRuntimePath()))
		panic(err)
	}
	// 程序退出前删除/var/run/casaos下的gateway.pid，management.url，static.url
	defer cleanupFiles(
		_state.GetRuntimePath(),
		pidFilename, external.ManagementURLFilename, external.StaticURLFilename,
	)

	defer func() {
		if _gateway != nil {
			if err := _gateway.Shutdown(context.Background()); err != nil {
				logger.Error("Failed to stop gateway", zap.Any("error", err))
			}
		}
	}()
	// 创建ctx，启动杀进程监听
	ctx, cancel := context.WithCancel(context.Background())
	kill := make(chan os.Signal, 1)
	signal.Notify(kill, syscall.SIGTERM, syscall.SIGINT)
	// 如果得到杀死进程的信号，则调用cancel
	go func() {
		<-kill
		cancel()
	}()

	go func() {
		<-_managementServiceReady // 阻塞等到managementService ready
		<-_gatewayServiceReady    // 阻塞等到gatewayService ready

		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that gateway is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that gateway is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}
	}()
	// 依赖注入
	app := fx.New(
		fx.Provide(func() *service.State { return _state }),
		fx.Provide(service.NewManagementService),
		fx.Provide(route.NewManagementRoute),
		fx.Provide(route.NewGatewayRoute),
		fx.Provide(route.NewStaticRoute),
		fx.Invoke(run),
	)

	if err := app.Start(ctx); err != nil {
		if err != context.Canceled {
			logger.Error("Failed to start gateway", zap.Any("error", err))
		}
	}
}

func run(
	lifecycle fx.Lifecycle,
	management *service.Management,
	managementRoute *route.ManagementRoute,
	gatewayRoute *route.GatewayRoute,
	staticRoute *route.StaticRoute,
) {
	// 从这开始利用fx依赖注入 开始注册网关
	// management server
	lifecycle.Append(
		fx.Hook{
			OnStart: func(context.Context) error {
				// 创建一个TCP监听器
				listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
				if err != nil {
					return err
				}

				// 创建一个管理服务的HTTP服务器
				managementServer := &http.Server{
					// Handler: 指定处理请求的处理器，此处使用了managementRoute.GetRoute()返回的处理函数。
					Handler:           managementRoute.GetRoute(),
					ReadHeaderTimeout: 5 * time.Second,
				}

				// 将监听器的地址写入文件
				urlFilePath, err := writeAddressFile(_state.GetRuntimePath(), external.ManagementURLFilename, "http://"+listener.Addr().String())
				if err != nil {
					return err
				}

				// 启动一个goroutine来运行管理服务
				go func() {
					logger.Info("Management service is listening...",
						zap.Any("address", listener.Addr().String()),
						zap.Any("filepath", urlFilePath),
					)
					err := managementServer.Serve(listener)
					if err != nil {
						logger.Error("management server error", zap.Any("error", err))
						os.Exit(1)
					}
				}()

				// 创建一个路由到"/v1/gateway/port"，目标为管理服务的地址
				if err := management.CreateRoute(&model.Route{
					Path:   "/v1/gateway/port",
					Target: "http://" + listener.Addr().String(),
				}); err != nil {
					return err
				}

				// 发送一个空值到"_managementServiceReady"通道
				_managementServiceReady <- struct{}{}

				return nil
			},
		},
	)
	// gateway service  启动gateway转发服务
	lifecycle.Append(
		fx.Hook{
			OnStart: func(ctx context.Context) error {
				route := gatewayRoute.GetRoute()

				if _state.GetGatewayPort() == "" {
					// check if a port is available starting from port 80/8080
					portsToCheck := []int{}
					for i := 80; i < 90; i++ {
						portsToCheck = append(portsToCheck, i)
					}

					for i := 8080; i < 8090; i++ {
						portsToCheck = append(portsToCheck, i)
					}

					port := ""
					for _, p := range portsToCheck {
						port = fmt.Sprintf("%d", p)
						logger.Info("Checking if port is available...", zap.Any("port", port))
						if listener, err := net.Listen("tcp", net.JoinHostPort("", port)); err == nil {
							if err = listener.Close(); err != nil {
								logger.Error("Failed to close listener", zap.Any("error", err), zap.Any("port", port))
								continue
							}
							break
						}
					}

					if port == "" {
						return errors.New("No port available for gateway to use")
					}

					if err := _state.SetGatewayPort(port); err != nil {
						return err
					}
				}

				_state.OnGatewayPortChange(func(port string) error {
					return reloadGateway(port, route)
				})

				if err := reloadGateway(_state.GetGatewayPort(), route); err != nil {
					return err
				}

				_gatewayServiceReady <- struct{}{}

				return nil
			},
		})

	// static web 访问web资源，跳转ui页面，/var/lib/casaos/www下的静态页面是Casaos-UI编译得到
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
			if err != nil {
				return err
			}

			staticServer := &http.Server{
				Handler:           staticRoute.GetRoute(),
				ReadHeaderTimeout: 5 * time.Second,
			}

			target := "http://" + listener.Addr().String()

			urlFilePath, err := writeAddressFile(_state.GetRuntimePath(), external.StaticURLFilename, target)
			if err != nil {
				return err
			}

			if err := management.CreateRoute(&model.Route{
				Path:   "/",
				Target: target,
			}); err != nil {
				return err
			}

			logger.Info(
				"Static web service is listening...",
				zap.Any("address", listener.Addr().String()),
				zap.Any("filepath", urlFilePath),
			)
			return staticServer.Serve(listener)
		},
	})
}

// 开启web服务转发，重启gateway
func reloadGateway(port string, route *http.ServeMux) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		return err
	}

	addr := listener.Addr().String()

	if _gateway != nil && _gateway.Addr == addr {
		logger.Info("Port is the same as current running gateway - no change is required")
		return nil
	}

	// start new gateway
	gatewayNew := &http.Server{
		Addr:              addr,
		Handler:           route,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		err := gatewayNew.Serve(listener)
		if err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				logger.Info("A gateway is stopped", zap.Any("address", gatewayNew.Addr))
				return
			}
			logger.Error("Error when serving a gateway", zap.Any("error", err), zap.Any("address", gatewayNew.Addr))
		}
	}()

	// test if gateway is running
	url := "http://" + addr + "/ping"
	if err := checkURLWithRetry(url, 10); err != nil {
		return err
	}

	logger.Info("New gateway is listening...", zap.Any("address", gatewayNew.Addr))

	// stop old gateway
	if _gateway != nil {
		gatewayOld := _gateway
		go func() {
			logger.Info("Stopping previous gateway in 1 seconds...", zap.Any("address", gatewayOld.Addr))
			time.Sleep(time.Second) // so that any request to the old gateway gets a response
			if err := gatewayOld.Shutdown(context.Background()); err != nil {
				logger.Error("Error when stopping previous gateway", zap.Any("error", err), zap.Any("address", gatewayOld.Addr))
			}
		}()
	}

	_gateway = gatewayNew

	return nil
}

func checkURLWithRetry(url string, retry uint) error {
	count := retry
	var err error

	for count >= 0 {
		logger.Info("Checking if service at URL is running...", zap.Any("url", url), zap.Any("retry", count))
		if err = checkURL(url); err != nil {
			time.Sleep(time.Second)
			count--
			continue
		}
		break
	}

	return err
}

func checkURL(url string) error {
	response, err := http2.Get(url, 5*time.Second)
	if err == nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusOK {
		return ErrCheckURLNotOK
	}

	return nil
}

func writePidFile(runtimePath string) (string, error) {
	filename := "gateway.pid"
	filepath := filepath.Join(runtimePath, filename)
	return filename, os.WriteFile(filepath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
}

func writeAddressFile(runtimePath string, filename string, address string) (string, error) {
	err := os.MkdirAll(runtimePath, 0o755)
	if err != nil {
		return "", err
	}

	filepath := filepath.Join(runtimePath, filename)
	return filepath, os.WriteFile(filepath, []byte(address), 0o600)
}

func cleanupFiles(runtimePath string, filenames ...string) {
	for _, filename := range filenames {
		err := os.Remove(filepath.Join(runtimePath, filename))
		if err != nil {
			logger.Error("Failed to cleanup file", zap.Any("error", err), zap.Any("filename", filename))
		}
	}
}

func checkPrequisites(state *service.State) error {
	path := state.GetRuntimePath()

	err := os.MkdirAll(path, 0o755)
	if err != nil {
		return fmt.Errorf("please ensure the owner of this service has write permission to that path %s", path)
	}

	return nil
}
