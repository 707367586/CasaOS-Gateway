package route

import (
	"crypto/ecdsa"
	"net/http"
	"os"

	"github.com/IceWhaleTech/CasaOS-Common/external"
	"github.com/IceWhaleTech/CasaOS-Common/middleware"
	"github.com/IceWhaleTech/CasaOS-Common/model"
	"github.com/IceWhaleTech/CasaOS-Common/utils/common_err"
	"github.com/IceWhaleTech/CasaOS-Common/utils/jwt"
	"github.com/IceWhaleTech/CasaOS-Gateway/service"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

type ManagementRoute struct {
	management *service.Management
}

func NewManagementRoute(management *service.Management) *ManagementRoute {
	return &ManagementRoute{
		management: management,
	}
}

func (m *ManagementRoute) GetRoute() *gin.Engine {
	// check if environment variable is set
	if ginMode, success := os.LookupEnv("GIN_MODE"); success {
		gin.SetMode(ginMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	//配置gin web 框架引擎
	r := gin.Default()
	r.Use(middleware.Cors())
	r.Use(gzip.Gzip(gzip.DefaultCompression))

	r.GET("/ping", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, gin.H{
			"message": "pong from management service",
		})
	})

	m.buildV1Group(r)

	return r
}

// buildV1Group 用于构建版本1的管理路由
func (m *ManagementRoute) buildV1Group(r *gin.Engine) {
	// 创建版本1的路由组
	v1Group := r.Group("/v1")

	// 使用中间件
	v1Group.Use()
	{
		// 构建版本1的路由
		m.buildV1RouteGroup(v1Group)
	}
}

// buildV1RouteGroup 用于构建版本1的路由组
func (m *ManagementRoute) buildV1RouteGroup(v1Group *gin.RouterGroup) {
	// 创建子路由组
	v1GatewayGroup := v1Group.Group("/gateway")

	// 使用中间件
	v1GatewayGroup.Use()
	{
		// 获取路由列表
		v1GatewayGroup.GET("/routes", func(ctx *gin.Context) {
			ctx.JSON(http.StatusOK, m.management.GetRoutes())
		})

		// 创建路由
		v1GatewayGroup.POST("/routes",
			jwt.ExceptLocalhost(func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(m.management.State.GetRuntimePath()) }),
			func(ctx *gin.Context) {
				var route *model.Route
				//json绑定实体类的方法
				err := ctx.ShouldBindJSON(&route)
				if err != nil {
					ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: err.Error(),
					})
					return
				}

				if err := m.management.CreateRoute(route); err != nil {
					ctx.JSON(http.StatusInternalServerError, model.Result{
						Success: common_err.SERVICE_ERROR,
						Message: err.Error(),
					})
					return
				}

				ctx.Status(http.StatusCreated)
			})

		// 获取端口号
		v1GatewayGroup.GET("/port", func(ctx *gin.Context) {
			ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: common_err.GetMsg(common_err.SUCCESS),
				Data:    m.management.GetGatewayPort(),
			})
		})

		// 修改端口号
		v1GatewayGroup.PUT("/port",
			jwt.ExceptLocalhost(func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(m.management.State.GetRuntimePath()) }),
			func(ctx *gin.Context) {
				var request *model.ChangePortRequest

				if err := ctx.ShouldBindJSON(&request); err != nil {
					ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.CLIENT_ERROR,
						Message: err.Error(),
					})
					return
				}

				if err := m.management.SetGatewayPort(request.Port); err != nil {
					ctx.JSON(http.StatusInternalServerError, model.Result{
						Success: common_err.SERVICE_ERROR,
						Message: err.Error(),
					})
					return
				}

				ctx.JSON(http.StatusOK, model.Result{
					Success: common_err.SUCCESS,
					Message: common_err.GetMsg(common_err.SUCCESS),
				})
			})
	}
}
