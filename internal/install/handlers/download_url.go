package handlers

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// downloadURLAndMaybeExtract 处理 source: "url" 的下载逻辑。
//
//   - 单文件（如 .onnx）：直接下载到 <localDir>/<filename>
//   - 压缩包（.tar.bz2 / .tar.gz / .tgz / .tar / .zip）：下载后就地解压到 localDir，
//     成功后删除压缩包；失败则保留压缩包给用户排查。
//
// 进度日志写入 logBuf；返回错误意味着该模型整体失败（外层会跳到下一个）。
func downloadURLAndMaybeExtract(rawURL, localDir string, logBuf io.Writer) error {
	if strings.TrimSpace(rawURL) == "" {
		return errors.New("url 为空")
	}
	if strings.TrimSpace(localDir) == "" {
		return errors.New("url 源必须指定 local_dir（解压/落盘目录）")
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", localDir, err)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("非法 url: %w", err)
	}
	filename := path.Base(u.Path)
	if filename == "" || filename == "/" {
		return fmt.Errorf("无法从 url 推断文件名: %s", rawURL)
	}
	dst := filepath.Join(localDir, filename)

	fmt.Fprintf(logBuf, "下载 %s\n  → %s\n", rawURL, dst)
	if err := httpDownload(rawURL, dst, logBuf); err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}

	// 如果是压缩包，就地展开到 localDir，成功后删除原包。
	if isArchive(filename) {
		fmt.Fprintf(logBuf, "解压 %s 到 %s\n", filename, localDir)
		if err := extractArchive(dst, localDir, logBuf); err != nil {
			return fmt.Errorf("解压失败（压缩包保留在 %s 供排查）: %w", dst, err)
		}
		_ = os.Remove(dst)
	}
	return nil
}

// httpDownload 是一个最小可靠的下载器：跟随重定向、流式写入、状态码校验。
// 不做断点续传——install 阶段的模型一次几十到几百 MB，失败重跑整步即可。
func httpDownload(rawURL, dst string, logBuf io.Writer) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	fmt.Fprintf(logBuf, "  写入 %d bytes\n", written)
	return nil
}

// isArchive 按扩展名判断是不是要解压的压缩包。
func isArchive(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.bz2"),
		strings.HasSuffix(lower, ".tbz2"),
		strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"),
		strings.HasSuffix(lower, ".zip"):
		return true
	}
	return false
}

// extractArchive 按扩展名分发到具体的解压实现。
// 所有实现都做防穿越（rejectTraversal）。
func extractArchive(archivePath, destDir string, logBuf io.Writer) error {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return extractTar(archivePath, destDir, "bz2", logBuf)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTar(archivePath, destDir, "gz", logBuf)
	case strings.HasSuffix(lower, ".tar"):
		return extractTar(archivePath, destDir, "", logBuf)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, destDir, logBuf)
	default:
		return fmt.Errorf("未知压缩格式: %s", archivePath)
	}
}

// extractTar 处理 .tar / .tar.gz / .tar.bz2。
// 这些 release tar 通常含一个顶层目录（如 sherpa-onnx-zipvoice-.../...），
// 我们直接展开到 destDir，最终结构变成 destDir/sherpa-onnx-zipvoice-.../...。
// 调用方若想把内容直接放到 destDir 根下，使用一个匹配顶层目录名的 local_dir 即可。
func extractTar(archivePath, destDir, compress string, logBuf io.Writer) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var stream io.Reader = f
	switch compress {
	case "bz2":
		stream = bzip2.NewReader(f)
	case "gz":
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return gzErr
		}
		defer gz.Close()
		stream = gz
	}

	tr := tar.NewReader(stream)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			count++
		case tar.TypeSymlink:
			// release 包里不应该出现 symlink；忽略，避免跨平台不一致
		}
	}
	fmt.Fprintf(logBuf, "  展开 %d 个文件\n", count)
	return nil
}

func extractZip(archivePath, destDir string, logBuf io.Writer) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	count := 0
	for _, f := range zr.File {
		target, err := safeJoin(destDir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()
			return err
		}
		_ = rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
		count++
	}
	fmt.Fprintf(logBuf, "  展开 %d 个文件\n", count)
	return nil
}

// safeJoin 防止 zip/tar 里的相对路径跳出 destDir（CVE-2018-6574 / Zip Slip）。
func safeJoin(destDir, name string) (string, error) {
	cleaned := filepath.Clean(name)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("非法压缩包条目: %s", name)
	}
	target := filepath.Join(destDir, cleaned)
	rel, err := filepath.Rel(destDir, target)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("非法压缩包条目（指向 destDir 外）: %s", name)
	}
	return target, nil
}
