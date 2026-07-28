package service

import (
	"fmt"

	"connectrpc.com/connect"

	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
)

// The server defaults for an unspecified format. PNG renders a page without
// artefacts on text; WEBP keeps a thumbnail small, which is the whole point of
// the cache holding it.
const (
	defaultRenderFormat    = hiramev1.ImageFormat_IMAGE_FORMAT_PNG
	defaultThumbnailFormat = hiramev1.ImageFormat_IMAGE_FORMAT_WEBP
)

// imageFormat is a resolved format: never UNSPECIFIED, and always one this
// system knows how to name, encode and serve.
type imageFormat struct {
	proto hiramev1.ImageFormat
	// name is the format's short name, used as the cache row's format column
	// and the object key's extension. It is the stored cache key, so it must
	// not change once objects exist under it.
	name        string
	mediaType   string
	rendererFmt gahakuclient.Format
}

var imageFormats = map[hiramev1.ImageFormat]imageFormat{
	hiramev1.ImageFormat_IMAGE_FORMAT_PNG: {
		proto: hiramev1.ImageFormat_IMAGE_FORMAT_PNG, name: "png",
		mediaType: "image/png", rendererFmt: gahakuclient.FormatPNG,
	},
	hiramev1.ImageFormat_IMAGE_FORMAT_JPEG: {
		proto: hiramev1.ImageFormat_IMAGE_FORMAT_JPEG, name: "jpeg",
		mediaType: "image/jpeg", rendererFmt: gahakuclient.FormatJPEG,
	},
	hiramev1.ImageFormat_IMAGE_FORMAT_WEBP: {
		proto: hiramev1.ImageFormat_IMAGE_FORMAT_WEBP, name: "webp",
		mediaType: "image/webp", rendererFmt: gahakuclient.FormatWEBP,
	},
	hiramev1.ImageFormat_IMAGE_FORMAT_AVIF: {
		proto: hiramev1.ImageFormat_IMAGE_FORMAT_AVIF, name: "avif",
		mediaType: "image/avif", rendererFmt: gahakuclient.FormatAVIF,
	},
}

// resolveFormat picks the format to render in, substituting fallback for
// UNSPECIFIED. A value outside the enum is INVALID_ARGUMENT rather than
// silently defaulted, because it means the client is built against a newer
// contract than this server implements.
func resolveFormat(
	requested hiramev1.ImageFormat,
	fallback hiramev1.ImageFormat,
) (imageFormat, error) {
	if requested == hiramev1.ImageFormat_IMAGE_FORMAT_UNSPECIFIED {
		requested = fallback
	}
	format, ok := imageFormats[requested]
	if !ok {
		return imageFormat{}, connect.NewError(
			connect.CodeInvalidArgument,
			fmt.Errorf("unsupported image format %s", requested),
		)
	}
	return format, nil
}

// imageBounds is a resolved size bound. Zero on an axis leaves it unbounded;
// the renderer never scales a page up, so a bound is a ceiling rather than a
// target.
type imageBounds struct {
	width  uint32
	height uint32
}

func (b imageBounds) message() *hiramev1.ImageSize {
	return &hiramev1.ImageSize{Width: b.width, Height: b.height}
}

// renderBounds is the bound a full-size render is held to. It is passed through
// as asked, capped so one request cannot ask Gahaku for an arbitrarily large
// raster.
func renderBounds(size *hiramev1.ImageSize) imageBounds {
	return imageBounds{
		width:  min(size.GetWidth(), maxRenderPixels),
		height: min(size.GetHeight(), maxRenderPixels),
	}
}

// maxRenderPixels caps either axis of a full-size render.
const maxRenderPixels = 5000

// Thumbnail sizing. Unset selects the default; anything else is rounded up to a
// multiple of the quantum and capped.
const (
	defaultThumbnailPixels = 192
	thumbnailQuantum       = 64
	maxThumbnailPixels     = 1024
)

// thumbnailBounds quantizes a requested thumbnail size.
//
// Quantization is what the proto allows a server to do so the cache does not
// fragment: the size is half of the cache key, and an unquantized one lets any
// caller mint an unbounded number of distinct entries for the same page. It
// happens before the key is built and the quantized values are what the
// response reports, so two callers whose requests quantize together agree about
// what they received.
func thumbnailBounds(size *hiramev1.ImageSize) imageBounds {
	return imageBounds{
		width:  quantizeThumbnailAxis(size.GetWidth()),
		height: quantizeThumbnailAxis(size.GetHeight()),
	}
}

func quantizeThumbnailAxis(value uint32) uint32 {
	if value == 0 {
		return defaultThumbnailPixels
	}
	if value >= maxThumbnailPixels {
		return maxThumbnailPixels
	}
	return (value + thumbnailQuantum - 1) / thumbnailQuantum * thumbnailQuantum
}
