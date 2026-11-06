package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

func syncBackup(ctx context.Context, c Config, dir string, info BackupInfo) error {
	if c.Sync.Type == "none" {
		return nil
	}
	if c.Sync.Type != "oss" {
		return fmt.Errorf("sync.type 仅支持 none/oss")
	}
	// SQL files precede the manifest. A missing backup.json identifies a run
	// whose transfer has not completed. No deletion/mirroring of remote files.
	var names []string
	for _, f := range info.Files {
		names = append(names, f.Name)
	}
	names = append(names, "backup.json")
	return syncOSSBackup(ctx, c, dir, info.ID, names)
}

func syncOSSBackup(ctx context.Context, c Config, dir, id string, names []string) error {
	s := c.Sync
	accessKeyID, accessKeySecret := s.OSSAccessKeyID, s.OSSAccessKeySecret
	if accessKeyID == "" {
		accessKeyID = os.Getenv("OSS_ACCESS_KEY_ID")
		accessKeySecret = os.Getenv("OSS_ACCESS_KEY_SECRET")
	}
	if accessKeyID == "" || accessKeySecret == "" {
		return fmt.Errorf("OSS AccessKey ID 和 Secret 必须同时配置，可填写 TOML 或 OSS_ACCESS_KEY_ID、OSS_ACCESS_KEY_SECRET 环境变量")
	}
	cfg := oss.LoadDefaultConfig().WithRegion(s.OSSRegion).
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, accessKeySecret, "")).
		WithHttpClient(&http.Client{Transport: http.DefaultTransport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("拒绝 OSS 重定向，请检查 oss_region 和 oss_endpoint")
		}})
	if s.OSSEndpoint != "" {
		cfg.WithEndpoint(s.OSSEndpoint)
	}
	uploader := oss.NewClient(cfg).NewUploader(func(options *oss.UploaderOptions) {
		// Upload-only credentials must never trigger AbortMultipartUpload.
		options.LeavePartsOnError = true
	})
	prefix := strings.Trim(s.OSSPrefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	prefix += c.Name + "/" + id + "/"
	for _, name := range names {
		// The deadline covers all parts and retries for this file.
		uploadCtx, cancel := context.WithTimeout(ctx, c.timeout())
		_, err := uploader.UploadFile(uploadCtx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(s.OSSBucket), Key: oss.Ptr(prefix + name),
		}, filepath.Join(dir, name))
		cancel()
		if err != nil {
			return fmt.Errorf("OSS 上传 %s: %w", name, err)
		}
	}
	fmt.Println("已同步:", "oss://"+s.OSSBucket+"/"+prefix)
	return nil
}
