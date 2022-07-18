package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	model "github.com/cloudreve/Cloudreve/v3/models"
	"github.com/cloudreve/Cloudreve/v3/pkg/auth"
	"github.com/cloudreve/Cloudreve/v3/pkg/cache"
	"github.com/cloudreve/Cloudreve/v3/pkg/conf"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/driver/cos"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/driver/onedrive"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/driver/oss"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/driver/s3"
	"github.com/cloudreve/Cloudreve/v3/pkg/request"
	"github.com/cloudreve/Cloudreve/v3/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v3/pkg/util"
	"github.com/gin-gonic/gin"
	cossdk "github.com/tencentyun/cos-go-sdk-v5"
)

// PathTestService 本地路径测试服务
type PathTestService struct {
	Path string `json:"path" binding:"required"`
}

// SlaveTestService 从机测试服务
type SlaveTestService struct {
	Secret string `json:"secret" binding:"required"`
	Server string `json:"server" binding:"required"`
}

// SlavePingService 从机相应ping
type SlavePingService struct {
	Callback string `json:"callback" binding:"required"`
}

// AddPolicyService 存储策略添加服务
type AddPolicyService struct {
	Policy model.Policy `json:"policy" binding:"required"`
}

// PolicyService 存储策略ID服务
type PolicyService struct {
	ID     uint   `uri:"id" json:"id" binding:"required"`
	Region string `json:"region"`
}

// Delete 删除存储策略
func (service *PolicyService) Delete() serializer.Response {
	// 禁止删除默认策略
	if service.ID == 1 {
		return serializer.Err(serializer.CodeDeleteDefaultPolicy, "", nil)
	}

	policy, err := model.GetPolicyByID(service.ID)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotExist, "", err)
	}

	// 检查是否有文件使用
	total := 0
	row := model.DB.Model(&model.File{}).Where("policy_id = ?", service.ID).
		Select("count(id)").Row()
	row.Scan(&total)
	if total > 0 {
		return serializer.Err(serializer.CodePolicyUsedByFiles, strconv.Itoa(total), nil)
	}

	// 检查用户组使用
	var groups []model.Group
	model.DB.Model(&model.Group{}).Where(
		"policies like ?",
		fmt.Sprintf("%%[%d]%%", service.ID),
	).Find(&groups)

	if len(groups) > 0 {
		return serializer.Err(serializer.CodePolicyUsedByGroups, strconv.Itoa(len(groups)), nil)
	}

	model.DB.Delete(&policy)
	policy.ClearCache()

	return serializer.Response{}
}

// Get 获取存储策略详情
func (service *PolicyService) Get() serializer.Response {
	policy, err := model.GetPolicyByID(service.ID)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotExist, "", err)
	}

	return serializer.Response{Data: policy}
}

// GetOAuth 获取 OneDrive OAuth 地址
func (service *PolicyService) GetOAuth(c *gin.Context) serializer.Response {
	policy, err := model.GetPolicyByID(service.ID)
	if err != nil || policy.Type != "onedrive" {
		return serializer.Err(serializer.CodePolicyNotExist, "", nil)
	}

	client, err := onedrive.NewClient(&policy)
	if err != nil {
		return serializer.Err(serializer.CodeInternalSetting, "Failed to initialize OneDrive client", err)
	}

	util.SetSession(c, map[string]interface{}{
		"onedrive_oauth_policy": policy.ID,
	})

	cache.Deletes([]string{policy.BucketName}, "onedrive_")

	return serializer.Response{Data: client.OAuthURL(context.Background(), []string{
		"offline_access",
		"files.readwrite.all",
	})}
}

// AddSCF 创建回调云函数
func (service *PolicyService) AddSCF() serializer.Response {
	policy, err := model.GetPolicyByID(service.ID)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotExist, "", nil)
	}

	if err := cos.CreateSCF(&policy, service.Region); err != nil {
		return serializer.ParamErr("Failed to create SCF function", err)
	}

	return serializer.Response{}
}

// AddCORS 创建跨域策略
func (service *PolicyService) AddCORS() serializer.Response {
	policy, err := model.GetPolicyByID(service.ID)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotExist, "", nil)
	}

	switch policy.Type {
	case "oss":
		handler, err := oss.NewDriver(&policy)
		if err != nil {
			return serializer.Err(serializer.CodeAddCORS, "", err)
		}
		if err := handler.CORS(); err != nil {
			return serializer.Err(serializer.CodeAddCORS, "", err)
		}
	case "cos":
		u, _ := url.Parse(policy.Server)
		b := &cossdk.BaseURL{BucketURL: u}
		handler := cos.Driver{
			Policy:     &policy,
			HTTPClient: request.NewClient(),
			Client: cossdk.NewClient(b, &http.Client{
				Transport: &cossdk.AuthorizationTransport{
					SecretID:  policy.AccessKey,
					SecretKey: policy.SecretKey,
				},
			}),
		}

		if err := handler.CORS(); err != nil {
			return serializer.Err(serializer.CodeAddCORS, "", err)
		}
	case "s3":
		handler, err := s3.NewDriver(&policy)
		if err != nil {
			return serializer.Err(serializer.CodeAddCORS, "", err)
		}

		if err := handler.CORS(); err != nil {
			return serializer.Err(serializer.CodeAddCORS, "", err)
		}
	default:
		return serializer.Err(serializer.CodePolicyNotAllowed, "", nil)
	}

	return serializer.Response{}
}

// Test 从机响应ping
func (service *SlavePingService) Test() serializer.Response {
	master, err := url.Parse(service.Callback)
	if err != nil {
		return serializer.ParamErr("无法解析主机站点地址，请检查主机 参数设置 - 站点信息 - 站点URL设置，"+err.Error(), nil)
	}

	controller, _ := url.Parse("/api/v3/site/ping")

	r := request.NewClient()
	res, err := r.Request(
		"GET",
		master.ResolveReference(controller).String(),
		nil,
		request.WithTimeout(time.Duration(10)*time.Second),
	).DecodeResponse()

	if err != nil {
		return serializer.ParamErr("从机无法向主机发送回调请求，请检查主机端 参数设置 - 站点信息 - 站点URL设置，并确保从机可以连接到此地址，"+err.Error(), nil)
	}

	version := conf.BackendVersion
	if conf.IsPro == "true" {
		version += "-pro"
	}
	if res.Data.(string) != version {
		return serializer.ParamErr("Cloudreve版本不一致，主机："+res.Data.(string)+"，从机："+version, nil)
	}

	return serializer.Response{}
}

// Test 测试从机通信
func (service *SlaveTestService) Test() serializer.Response {
	slave, err := url.Parse(service.Server)
	if err != nil {
		return serializer.ParamErr("Failed to parse slave node server URL: "+err.Error(), nil)
	}

	controller, _ := url.Parse("/api/v3/slave/ping")

	// 请求正文
	body := map[string]string{
		"callback": model.GetSiteURL().String(),
	}
	bodyByte, _ := json.Marshal(body)

	r := request.NewClient()
	res, err := r.Request(
		"POST",
		slave.ResolveReference(controller).String(),
		bytes.NewReader(bodyByte),
		request.WithTimeout(time.Duration(10)*time.Second),
		request.WithCredential(
			auth.HMACAuth{SecretKey: []byte(service.Secret)},
			int64(model.GetIntSetting("slave_api_timeout", 60)),
		),
	).DecodeResponse()
	if err != nil {
		return serializer.ParamErr("Failed to connect to slave node: "+err.Error(), nil)
	}

	if res.Code != 0 {
		return serializer.ParamErr("Successfully connected to slave node, but slave returns: "+res.Msg, nil)
	}

	return serializer.Response{}
}

// Add 添加存储策略
func (service *AddPolicyService) Add() serializer.Response {
	if service.Policy.Type != "local" && service.Policy.Type != "remote" {
		service.Policy.DirNameRule = strings.TrimPrefix(service.Policy.DirNameRule, "/")
	}

	if service.Policy.ID > 0 {
		if err := model.DB.Save(&service.Policy).Error; err != nil {
			return serializer.DBErr("Failed to save policy", err)
		}
	} else {
		if err := model.DB.Create(&service.Policy).Error; err != nil {
			return serializer.DBErr("Failed to create policy", err)
		}
	}

	service.Policy.ClearCache()

	return serializer.Response{Data: service.Policy.ID}
}

// Test 测试本地路径
func (service *PathTestService) Test() serializer.Response {
	policy := model.Policy{DirNameRule: service.Path}
	path := policy.GeneratePath(1, "/My File")
	path = filepath.Join(path, "test.txt")
	file, err := util.CreatNestedFile(util.RelativePath(path))
	if err != nil {
		return serializer.ParamErr(fmt.Sprintf("Failed to create \"%s\": %s", path, err.Error()), nil)
	}

	file.Close()
	os.Remove(path)

	return serializer.Response{}
}

// Policies 列出存储策略
func (service *AdminListService) Policies() serializer.Response {
	var res []model.Policy
	total := 0

	tx := model.DB.Model(&model.Policy{})
	if service.OrderBy != "" {
		tx = tx.Order(service.OrderBy)
	}

	for k, v := range service.Conditions {
		tx = tx.Where(k+" = ?", v)
	}

	// 计算总数用于分页
	tx.Count(&total)

	// 查询记录
	tx.Limit(service.PageSize).Offset((service.Page - 1) * service.PageSize).Find(&res)

	// 统计每个策略的文件使用
	statics := make(map[uint][2]int, len(res))
	for i := 0; i < len(res); i++ {
		total := [2]int{}
		row := model.DB.Model(&model.File{}).Where("policy_id = ?", res[i].ID).
			Select("count(id),sum(size)").Row()
		row.Scan(&total[0], &total[1])
		statics[res[i].ID] = total
	}

	return serializer.Response{Data: map[string]interface{}{
		"total":   total,
		"items":   res,
		"statics": statics,
	}}
}
