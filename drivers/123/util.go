package _123

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	jsoniter "github.com/json-iterator/go"
	log "github.com/sirupsen/logrus"
)

// do others that not defined in Driver interface

const (
	Api                = "https://www.123pan.com/api"
	AApi               = "https://www.123pan.com/a/api"
	BApi               = "https://www.123pan.com/b/api"
	LoginApi           = "https://login.123pan.com/api"
	MainApi            = BApi
	SignIn             = LoginApi + "/user/sign_in"
	Logout             = MainApi + "/user/logout"
	UserInfo           = MainApi + "/user/info"
	FileList           = MainApi + "/file/list/new"
	DownloadInfo       = MainApi + "/file/download_info"
	Mkdir              = MainApi + "/file/upload_request"
	Move               = MainApi + "/file/mod_pid"
	Rename             = MainApi + "/file/rename"
	Trash              = MainApi + "/file/trash"
	UploadRequest      = MainApi + "/file/upload_request"
	UploadComplete     = MainApi + "/file/upload_complete"
	S3PreSignedUrls    = MainApi + "/file/s3_repare_upload_parts_batch"
	S3Auth             = MainApi + "/file/s3_upload_object/auth"
	UploadCompleteV2   = MainApi + "/file/upload_complete/v2"
	S3Complete         = MainApi + "/file/s3_complete_multipart_upload"
	//AuthKeySalt = "8-8D$sL8gPjom7bk#cY"
)

func signPath(path string, os string, version string) (k string, v string) {
	table := []byte{'a', 'd', 'e', 'f', 'g', 'h', 'l', 'm', 'y', 'i', 'j', 'n', 'o', 'p', 'k', 'q', 'r', 's', 't', 'u', 'b', 'c', 'v', 'w', 's', 'z'}
	random := fmt.Sprintf("%.f", math.Round(1e7*rand.Float64()))
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	timestamp := fmt.Sprint(now.Unix())
	nowStr := []byte(now.Format("200601021504"))
	for i := 0; i < len(nowStr); i++ {
		nowStr[i] = table[nowStr[i]-48]
	}
	timeSign := fmt.Sprint(crc32.ChecksumIEEE(nowStr))
	data := strings.Join([]string{timestamp, random, path, os, version, timeSign}, "|")
	dataSign := fmt.Sprint(crc32.ChecksumIEEE([]byte(data)))
	return timeSign, strings.Join([]string{timestamp, random, dataSign}, "-")
}

func GetApi(rawUrl string) string {
	u, _ := url.Parse(rawUrl)
	query := u.Query()
	query.Add(signPath(u.Path, "web", "3"))
	u.RawQuery = query.Encode()
	return u.String()
}

func (d *Pan123) login() error {
	var body base.Json
	if utils.IsEmailFormat(d.Username) {
		body = base.Json{
			"mail":     d.Username,
			"password": d.Password,
			"type":     2,
		}
	} else {
		body = base.Json{
			"passport": d.Username,
			"password": d.Password,
			"remember": true,
		}
	}

	req := base.RestyClient.R().
		SetHeaders(map[string]string{
			"origin":      "https://www.123pan.com",
			"referer":     "https://www.123pan.com/",
			"user-agent":  "Dart/2.19(dart:io)-openlist",
			"platform":    "web",
			"app-version": "3",
		}).
		SetBody(body)

	// 添加全局IP伪装headers
	if d.FakeIP != "" {
		req.SetHeaders(map[string]string{
			"X-Real-IP":       d.FakeIP,
			"X-Forwarded-For": d.FakeIP,
		})
	}

	res, err := req.Post(SignIn)
	if err != nil {
		return err
	}

	if utils.Json.Get(res.Body(), "code").ToInt() != 200 {
		err = fmt.Errorf(utils.Json.Get(res.Body(), "message").ToString())
	} else {
		d.AccessToken = utils.Json.Get(res.Body(), "data", "token").ToString()
	}
	return err
}

func (d *Pan123) Request(url string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	isRetry := false
do:
	req := base.RestyClient.R()
	
	// 设置基础headers
	baseHeaders := map[string]string{
		"origin":        "https://www.123pan.com",
		"referer":       "https://www.123pan.com/",
		"authorization": "Bearer " + d.AccessToken,
		"user-agent":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) openlist-client",
		"platform":      "web",
		"app-version":   "3",
	}
	
	// 添加全局IP伪装headers
	if d.FakeIP != "" {
		baseHeaders["X-Real-IP"] = d.FakeIP
		baseHeaders["X-Forwarded-For"] = d.FakeIP
	}
	
	req.SetHeaders(baseHeaders)

	if callback != nil {
		callback(req)
	}

	if resp != nil {
		req.SetResult(resp)
	}

	res, err := req.Execute(method, GetApi(url))
	if err != nil {
		return nil, err
	}

	body := res.Body()
	code := utils.Json.Get(body, "code").ToInt()
	if code != 0 {
		if !isRetry && code == 401 {
			err := d.login()
			if err != nil {
				return nil, err
			}
			isRetry = true
			goto do
		}
		return nil, errors.New(jsoniter.Get(body, "message").ToString())
	}

	return body, nil
}

func (d *Pan123) getFiles(ctx context.Context, parentId string, name string) ([]File, error) {
	page := 1
	total := 0
	res := make([]File, 0)
	// 2024-02-06 fix concurrency by 123pan
	for {
		if err := d.APIRateLimit(ctx, FileList); err != nil {
			return nil, err
		}

		var resp Files
		query := map[string]string{
			"driveId":              "0",
			"limit":                "100",
			"next":                 "0",
			"orderBy":              "file_id",
			"orderDirection":       "desc",
			"parentFileId":         parentId,
			"trashed":              "false",
			"SearchData":           "",
			"Page":                 strconv.Itoa(page),
			"OnlyLookAbnormalFile": "0",
			"event":                "homeListFile",
			"operateType":          "4",
			"inDirectSpace":        "false",
		}

		_res, err := d.Request(FileList, http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(query)
		}, &resp)
		if err != nil {
			return nil, err
		}

		log.Debug(string(_res))
		page++
		res = append(res, resp.Data.InfoList...)
		total = resp.Data.Total
		if len(resp.Data.InfoList) == 0 || resp.Data.Next == "-1" {
			break
		}
	}

	if len(res) != total {
		log.Warnf("incorrect file count from remote at %s: expected %d, got %d", name, total, len(res))
	}

	return res, nil
}
