package _123

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

// 全局变量，您只需要在这里修改IP地址即可实现全局伪装
var FAKE_IP = "120.229.187.66"

type Pan123 struct {
	model.Storage
	Addition
	apiRateLimit sync.Map
	client       *resty.Client
}

func (d *Pan123) Config() driver.Config {
	return config
}

func (d *Pan123) GetAddition() driver.Additional {
	return &d.Addition
}

// 统一的请求头部设置方法
func (d *Pan123) getGlobalHeaders() map[string]string {
	return map[string]string{
		"X-Real-IP":       FAKE_IP,
		"X-Forwarded-For": FAKE_IP,
		"user-agent":      "123pan/2.5.5(Android_13.1.2;Vivo)",
		"platform":        "android",
		"app-version":     "78",
		"x-app-version":   "2.5.5",
	}
}

// 修改Request方法，实现全局IP伪装
func (d *Pan123) Request(url string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	// 应用速率限制
	if err := d.APIRateLimit(context.Background(), url); err != nil {
		return nil, err
	}
	
	// 创建请求并设置全局伪装头部
	req := d.client.R()
	req.SetHeaders(d.getGlobalHeaders())
	
	// 如果有回调函数，执行它（可能会覆盖某些头部）
	if callback != nil {
		callback(req)
	}
	
	// 执行请求
	var res *resty.Response
	var err error
	
	switch method {
	case http.MethodGet:
		res, err = req.Get(url)
	case http.MethodPost:
		res, err = req.Post(url)
	case http.MethodPut:
		res, err = req.Put(url)
	case http.MethodDelete:
		res, err = req.Delete(url)
	default:
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
	
	if err != nil {
		return nil, err
	}
	
	// 处理响应
	if resp != nil {
		if err := utils.Json.Unmarshal(res.Body(), resp); err != nil {
			return nil, err
		}
	}
	
	return res.Body(), nil
}

func (d *Pan123) Init(ctx context.Context) error {
	_, err := d.Request(UserInfo, http.MethodGet, nil, nil)
	return err
}

func (d *Pan123) Drop(ctx context.Context) error {
	_, _ = d.Request(Logout, http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{})
	}, nil)
	return nil
}

func (d *Pan123) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetID(), dir.GetName())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return src, nil
	})
}

func (d *Pan123) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if f, ok := file.(File); ok {
		data := base.Json{
			"driveId":   0,
			"etag":      f.Etag,
			"fileId":    f.FileId,
			"fileName":  f.FileName,
			"s3keyFlag": f.S3KeyFlag,
			"size":      f.Size,
			"type":      f.Type,
		}
		
		resp, err := d.Request(DownloadInfo, http.MethodPost, func(req *resty.Request) {
			req.SetBody(data)
			// 如果需要特殊的IP处理，可以在这里覆盖X-Forwarded-For
			if !utils.IsLocalIPAddr(args.IP) {
				req.SetHeader("X-Forwarded-For", args.IP)
			}
		}, nil)
		
		if err != nil {
			return nil, err
		}
		downloadUrl := utils.Json.Get(resp, "data", "DownloadUrl").ToString()
		u, err := url.Parse(downloadUrl)
		if err != nil {
			return nil, err
		}
		nu := u.Query().Get("params")
		if nu != "" {
			du, _ := base64.StdEncoding.DecodeString(nu)
			u, err = url.Parse(string(du))
			if err != nil {
				return nil, err
			}
		}
		u_ := u.String()
		log.Debug("download url: ", u_)
		res, err := base.NoRedirectClient.R().SetHeader("Referer", "https://www.123pan.com/").Get(u_)
		if err != nil {
			return nil, err
		}
		log.Debug(res.String())
		link := model.Link{
			URL: u_,
		}
		log.Debugln("res code: ", res.StatusCode())
		if res.StatusCode() == 302 {
			link.URL = res.Header().Get("location")
		} else if res.StatusCode() < 300 {
			link.URL = utils.Json.Get(res.Body(), "data", "redirect_url").ToString()
		}
		link.Header = http.Header{
			"Referer": []string{"https://www.123pan.com/"},
		}
		return &link, nil
	} else {
		return nil, fmt.Errorf("can't convert obj")
	}
}

func (d *Pan123) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	data := base.Json{
		"driveId":      0,
		"etag":         "",
		"fileName":     dirName,
		"parentFileId": parentDir.GetID(),
		"size":         0,
		"type":         1,
	}
	_, err := d.Request(Mkdir, http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Pan123) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := base.Json{
		"fileIdList":   []base.Json{{"FileId": srcObj.GetID()}},
		"parentFileId": dstDir.GetID(),
	}
	_, err := d.Request(Move, http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Pan123) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"driveId":  0,
		"fileId":   srcObj.GetID(),
		"fileName": newName,
	}
	_, err := d.Request(Rename, http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Pan123) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *Pan123) Remove(ctx context.Context, obj model.Obj) error {
	if f, ok := obj.(File); ok {
		data := base.Json{
			"driveId":           0,
			"operation":         true,
			"fileTrashInfoList": []File{f},
		}
		_, err := d.Request(Trash, http.MethodPost, func(req *resty.Request) {
			req.SetBody(data)
		}, nil)
		return err
	} else {
		return fmt.Errorf("can't convert obj")
	}
}

func (d *Pan123) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	etag := file.GetHash().GetHash(utils.MD5)
	var err error
	if len(etag) < utils.MD5.Width {
		cacheFileProgress := model.UpdateProgressWithRange(up, 0, 50)
		up = model.UpdateProgressWithRange(up, 50, 100)
		_, etag, err = stream.CacheFullInTempFileAndHash(file, cacheFileProgress, utils.MD5)
		if err != nil {
			return err
		}
	}
	data := base.Json{
		"driveId":      0,
		"duplicate":    2, // 2->覆盖 1->重命名 0->默认
		"etag":         strings.ToLower(etag),
		"fileName":     file.GetName(),
		"parentFileId": dstDir.GetID(),
		"size":         file.GetSize(),
		"type":         0,
	}
	var resp UploadResp
	res, err := d.Request(UploadRequest, http.MethodPost, func(req *resty.Request) {
		req.SetBody(data).SetContext(ctx)
	}, &resp)
	if err != nil {
		return err
	}
	log.Debugln("upload request res: ", string(res))
	if resp.Data.Reuse || resp.Data.Key == "" {
		return nil
	}
	if resp.Data.AccessKeyId == "" || resp.Data.SecretAccessKey == "" || resp.Data.SessionToken == "" {
		err = d.newUpload(ctx, &resp, file, up)
		return err
	} else {
		cfg := &aws.Config{
			Credentials:      credentials.NewStaticCredentials(resp.Data.AccessKeyId, resp.Data.SecretAccessKey, resp.Data.SessionToken),
			Region:           aws.String("123pan"),
			Endpoint:         aws.String(resp.Data.EndPoint),
			S3ForcePathStyle: aws.Bool(true),
		}
		s, err := session.NewSession(cfg)
		if err != nil {
			return err
		}
		uploader := s3manager.NewUploader(s)
		if file.GetSize() > s3manager.MaxUploadParts*s3manager.DefaultUploadPartSize {
			uploader.PartSize = file.GetSize() / (s3manager.MaxUploadParts - 1)
		}
		input := &s3manager.UploadInput{
			Bucket: &resp.Data.Bucket,
			Key:    &resp.Data.Key,
			Body: driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
				Reader:         file,
				UpdateProgress: up,
			}),
		}
		_, err = uploader.UploadWithContext(ctx, input)
		if err != nil {
			return err
		}
	}
	_, err = d.Request(UploadComplete, http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"fileId": resp.Data.FileId,
		}).SetContext(ctx)
	}, nil)
	return err
}

func (d *Pan123) APIRateLimit(ctx context.Context, api string) error {
	value, _ := d.apiRateLimit.LoadOrStore(api,
		rate.NewLimiter(rate.Every(700*time.Millisecond), 1))
	limiter := value.(*rate.Limiter)

	return limiter.Wait(ctx)
}

var _ driver.Driver = (*Pan123)(nil)
