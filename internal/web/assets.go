package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/labstack/echo/v4"

	symphony "github.com/digitaldrywood/symphony"
	"github.com/digitaldrywood/symphony/internal/web/templates"
)

const (
	staticURLPrefix        = "/static/"
	defaultStylesheetPath  = "/static/css/output.css"
	headerETag             = "ETag"
	fingerprintHashLength  = 12
	immutableCacheControl  = "public, max-age=31536000, immutable"
	revalidateCacheControl = "no-cache"
)

type staticAssets struct {
	fsys          fs.FS
	originals     map[string]staticAsset
	fingerprinted map[string]staticAsset
}

type staticAsset struct {
	urlPath       string
	filePath      string
	fingerprinted string
	etag          string
}

func newStaticAssets(staticDir string) staticAssets {
	fsys := symphony.StaticFS()
	if strings.TrimSpace(staticDir) != "" {
		fsys = os.DirFS(staticDir)
	}

	assets := staticAssets{
		fsys:          fsys,
		originals:     make(map[string]staticAsset),
		fingerprinted: make(map[string]staticAsset),
	}
	assets.index()
	return assets
}

func (a staticAssets) templatePaths() templates.AssetPaths {
	return templates.AssetPaths{
		Stylesheet: a.assetPath(defaultStylesheetPath),
	}
}

func (a staticAssets) assetPath(original string) string {
	if asset, ok := a.originals[cleanStaticPath(original)]; ok {
		return asset.fingerprinted
	}
	return original
}

func (a staticAssets) serve(c echo.Context) (err error) {
	requestPath := cleanStaticPath(staticURLPrefix + strings.TrimPrefix(c.Param("*"), "/"))
	if !strings.HasPrefix(requestPath, staticURLPrefix) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	asset, fingerprinted := a.resolve(requestPath)
	if asset.filePath == "" {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	file, err := a.fsys.Open(asset.filePath)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	header := c.Response().Header()
	if fingerprinted {
		header.Set(echo.HeaderCacheControl, immutableCacheControl)
	} else {
		header.Set(echo.HeaderCacheControl, revalidateCacheControl)
	}
	if asset.etag != "" {
		header.Set(headerETag, asset.etag)
	}

	reader, ok := file.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	http.ServeContent(c.Response(), c.Request(), path.Base(asset.urlPath), stat.ModTime(), reader)
	return nil
}

func (a staticAssets) resolve(requestPath string) (staticAsset, bool) {
	if asset, ok := a.fingerprinted[requestPath]; ok {
		return asset, true
	}
	if asset, ok := a.originals[requestPath]; ok {
		return asset, false
	}
	return staticAsset{
		urlPath:  requestPath,
		filePath: staticFilePath(requestPath),
	}, false
}

func (a staticAssets) index() {
	if err := fs.WalkDir(a.fsys, ".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(a.fsys, filePath)
		if err != nil {
			return nil
		}

		hash := contentHash(data)
		urlPath := staticURLPrefix + filePath
		fingerprinted := fingerprintedStaticPath(urlPath, hash)
		asset := staticAsset{
			urlPath:       urlPath,
			filePath:      filePath,
			fingerprinted: fingerprinted,
			etag:          `"sha256-` + hash + `"`,
		}
		a.originals[urlPath] = asset
		a.fingerprinted[fingerprinted] = asset
		return nil
	}); err != nil {
		return
	}
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:fingerprintHashLength]
}

func fingerprintedStaticPath(urlPath string, hash string) string {
	ext := path.Ext(urlPath)
	if ext == "" {
		return urlPath + "." + hash
	}
	return strings.TrimSuffix(urlPath, ext) + "." + hash + ext
}

func cleanStaticPath(value string) string {
	return path.Clean("/" + strings.TrimPrefix(value, "/"))
}

func staticFilePath(urlPath string) string {
	if !strings.HasPrefix(urlPath, staticURLPrefix) {
		return ""
	}
	return strings.TrimPrefix(urlPath, staticURLPrefix)
}
