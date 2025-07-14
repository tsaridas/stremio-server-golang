# Stremio Server - Go Implementation

This is a Go rewrite of the original Node.js Stremio server, providing the same functionality with improved performance and resource efficiency.

## Features

- **Torrent Streaming**: Full torrent support with streaming capabilities
- **FFmpeg Integration**: Video transcoding, HLS streaming, and hardware acceleration
- **HTTP/HTTPS Servers**: Dual server support on ports 11470 (HTTP) and 12470 (HTTPS)
- **Range Requests**: Support for partial content requests for video streaming
- **CORS Support**: Configurable CORS headers
- **Basic Authentication**: Optional HTTP basic auth
- **WebSocket Support**: Real-time updates via WebSocket connections
- **HLS Streaming**: Support for HTTP Live Streaming with real-time transcoding
- **Proxy Support**: HTTP proxy functionality
- **API Endpoints**: Complete REST API for torrent management and video processing

## API Endpoints

### Torrent Management
- `POST /{infoHash}/create` - Create a new torrent with infoHash in URL path and custom configuration
- `GET /list` - List all active torrents
- `GET /stats/{infoHash}` - Get torrent statistics
- `GET /stats/{infoHash}/{fileIndex}/stats.json` - Get detailed file statistics
- `DELETE /remove/{infoHash}` - Remove a torrent

### Creating a Torrent with Custom Configuration

The `/create` and `/{infoHash}/create` endpoints accept a JSON payload with torrent information, peer search configuration, and tracker sources:

#### Original `/create` endpoint:
```json
{
  "torrent": {
    "infoHash": "04b4000d88481e39b1fa486d064262ba9b7cbe8a"
  },
  "peerSearch": {
    "sources": [
      "dht:04b4000d88481e39b1fa486d064262ba9b7cbe8a",
      "tracker:udp://tracker.opentrackr.org:1337/announce",
      "tracker:udp://open.tracker.cl:1337/announce",
      "tracker:udp://open.demonii.com:1337/announce"
    ],
    "min": 40,
    "max": 200
  },
  "guessFileIdx": {}
}
```

#### New `/{infoHash}/create` endpoint:
```json
{
  "torrent": {
    "infoHash": "b081ff8cd259906524817b21e2476ee83e37a4aa"
  },
  "peerSearch": {
    "sources": [
      "dht:b081ff8cd259906524817b21e2476ee83e37a4aa",
      "tracker:udp://tracker.opentrackr.org:1337/announce"
    ],
    "min": 40,
    "max": 200
  },
  "guessFileIdx": {}
}
```

**Parameters:**
- `torrent.infoHash`: The SHA1 hash of the torrent (required for `/create`, optional for `/{infoHash}/create`)
- `peerSearch.sources`: Array of peer discovery sources
  - `dht:{infoHash}`: Enable DHT for the specific torrent
  - `tracker:{url}`: Add tracker URL for peer discovery
- `peerSearch.min`: Minimum number of peers to maintain
- `peerSearch.max`: Maximum number of peers to connect to
- `guessFileIdx`: Reserved for future use

**Examples:**

**Original endpoint:**
```bash
curl -X POST http://localhost:11470/create \
  -H "Content-Type: application/json" \
  -d '{
    "torrent": {
      "infoHash": "04b4000d88481e39b1fa486d064262ba9b7cbe8a"
    },
    "peerSearch": {
      "sources": [
        "dht:04b4000d88481e39b1fa486d064262ba9b7cbe8a",
        "tracker:udp://tracker.opentrackr.org:1337/announce"
      ],
      "min": 40,
      "max": 200
    },
    "guessFileIdx": {}
  }'
```

**New endpoint with infoHash in URL:**
```bash
curl -X POST http://localhost:11470/b081ff8cd259906524817b21e2476ee83e37a4aa/create \
  -H "Content-Type: application/json" \
  -d '{
    "torrent": {},
    "peerSearch": {
      "sources": [
        "dht:b081ff8cd259906524817b21e2476ee83e37a4aa",
        "tracker:udp://tracker.opentrackr.org:1337/announce"
      ],
      "min": 40,
      "max": 200
    },
    "guessFileIdx": {}
  }'
```

### Streaming
- `GET /stream/{infoHash}/{fileIndex}` - Stream a torrent file
- `GET /hlsv2/{infoHash}/{fileIndex}/master.m3u8` - HLS master playlist
- `GET /hlsv2/{infoHash}/{fileIndex}/stream.m3u8` - HLS stream playlist
- `GET /hlsv2/{infoHash}/{fileIndex}/stream-{quality}.m3u8` - HLS quality-specific playlist
- `GET /hlsv2/{infoHash}/{fileIndex}/stream-{quality}/{segment}.ts` - HLS segment

### FFmpeg Operations
- `GET /hwaccel-profiler` - Hardware acceleration information
- `POST /transcode` - Video transcoding
- `GET /probe` - Video information and metadata
- `GET /thumb.jpg` - Generate video thumbnails

### Server Information
- `GET /status` - Server status and uptime
- `GET /network-info` - Network information
- `GET /device-info` - Device information
- `GET /settings` - Server settings

### Other
- `GET /proxy` - HTTP proxy
- `GET /subtitles` - Subtitle handling
- `GET /casting` - Casting support
- `GET /local-addon` - Local addon support
- `GET /ws` - WebSocket endpoint

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_PORT` | 11470 | HTTP server port |
| `HTTPS_PORT` | 12470 | HTTPS server port |
| `APP_PATH` | `/root/.stremio-server` | Application data path |
| `NO_CORS` | false | Disable CORS headers |
| `USERNAME` | - | Basic auth username |
| `PASSWORD` | - | Basic auth password |
| `LOG_LEVEL` | info | Logging level (info, warn, error) |

### FFmpeg Configuration
| Variable | Default | Description |
|----------|---------|-------------|
| `FFMPEG_HARDWARE_ACCEL` | true | Enable hardware acceleration |
| `FFMPEG_DEBUG` | false | Enable FFmpeg debug logging |
| `FFMPEG_HORSEPOWER` | 0.75 | Transcoding performance factor |
| `FFMPEG_MAX_BITRATE` | 0 | Maximum bitrate for transcoding |
| `FFMPEG_CONCURRENCY` | 1 | Number of concurrent transcoding jobs |
| `FFMPEG_MAX_WIDTH` | 1920 | Maximum video width for transcoding |
| `FFMPEG_PROFILE` | - | Custom FFmpeg profile |

## Building

### Prerequisites
- Go 1.21 or later
- Git

### Local Build
```bash
# Clone the repository
git clone <repository-url>
cd stremio-server

# Download dependencies
go mod download

# Build the application
go build -o stremio-server .

# Run the server
./stremio-server
```

### Docker Build
```bash
# Build the Docker image
docker build -f Dockerfile.go -t stremio-server-go .

# Run the container
docker run -d \
  --name stremio-server-go \
  -p 11470:11470 \
  -p 12470:12470 \
  -v ./stremio-data:/root/.stremio-server \
  -e NO_CORS=1 \
  stremio-server-go
```

## Usage Examples

### Creating a Torrent
```bash
curl -X POST http://localhost:11470/create \
  -H "Content-Type: application/json" \
  -d '{"magnetURI": "magnet:?xt=urn:btih:..."}'
```

### Streaming a File
```bash
curl -H "Range: bytes=0-1048575" \
  http://localhost:11470/stream/{infoHash}/0
```

### Getting Torrent Stats
```bash
curl http://localhost:11470/stats/{infoHash}
```

### Getting File Stats
```bash
curl http://localhost:11470/stats/{infoHash}/0/stats.json
```

### FFmpeg Operations

#### Hardware Acceleration Info
```bash
curl http://localhost:11470/hwaccel-profiler
```

#### Video Transcoding
```bash
curl -X POST http://localhost:11470/transcode \
  -H "Content-Type: application/json" \
  -d '{
    "inputPath": "/path/to/input.mp4",
    "outputPath": "/path/to/output.mp4",
    "options": {
      "videoCodec": "h264",
      "audioCodec": "aac",
      "videoBitRate": 2000,
      "audioBitRate": 128,
      "width": 1920,
      "height": 1080,
      "hardwareAccel": true
    }
  }'
```

#### Video Probing
```bash
curl "http://localhost:11470/probe?path=/path/to/video.mp4"
```

The probe endpoint returns detailed media information in the following format:
```json
{
    "format": {
        "name": "matroska,webm",
        "duration": 1386.144
    },
    "streams": [
        {
            "id": 0,
            "index": 0,
            "track": "video",
            "codec": "hevc",
            "streamBitRate": 0,
            "streamMaxBitRate": 0,
            "startTime": 0,
            "startTimeTs": 0,
            "timescale": 1000,
            "width": 1280,
            "height": 720,
            "frameRate": 23.976023976023978,
            "numberOfFrames": null,
            "isHdr": false,
            "isDoVi": false,
            "hasBFrames": true,
            "formatBitRate": 1097224,
            "formatMaxBitRate": 0,
            "bps": 5589998,
            "numberOfBytes": 968532259,
            "formatDuration": 1386.144
        },
        {
            "id": 0,
            "index": 1,
            "track": "audio",
            "codec": "eac3",
            "streamBitRate": 256000,
            "streamMaxBitRate": 0,
            "startTime": 0,
            "startTimeTs": 0,
            "timescale": 1000,
            "sampleRate": 48000,
            "channels": 6,
            "channelLayout": "5.1(side)",
            "title": null,
            "language": "eng"
        },
        {
            "id": 0,
            "index": 2,
            "track": "subtitle",
            "codec": "ass",
            "streamBitRate": 0,
            "streamMaxBitRate": 0,
            "startTime": 0,
            "startTimeTs": 0,
            "timescale": 1000,
            "title": "English [SDH]",
            "language": "eng"
        }
    ],
    "samples": {}
}
```

#### Thumbnail Generation
```bash
curl "http://localhost:11470/thumb.jpg?path=/path/to/video.mp4&time=10.5"
```

## Performance Benefits

Compared to the Node.js version:

- **Lower Memory Usage**: Go's efficient memory management
- **Better Concurrency**: Goroutines for handling multiple connections
- **Faster Startup**: Compiled binary starts faster than interpreted JavaScript
- **Smaller Image Size**: Alpine-based Docker image is more compact
- **Better Resource Management**: Automatic garbage collection and memory optimization
- **Hardware Acceleration**: Full FFmpeg hardware acceleration support (VAAPI, CUDA, VideoToolbox)

## Hardware Acceleration

The Go implementation includes full FFmpeg hardware acceleration support:

### Supported Platforms
- **Linux**: VAAPI (Intel/AMD), CUDA (NVIDIA)
- **macOS**: VideoToolbox
- **Windows**: DirectX 11, CUDA

### Enabling Hardware Acceleration

#### Docker with GPU Support
```bash
# For Intel/AMD GPUs (VAAPI)
docker run -d \
  --device /dev/dri:/dev/dri \
  -e FFMPEG_HARDWARE_ACCEL=1 \
  stremio-server-go

# For NVIDIA GPUs (CUDA)
docker run -d \
  --gpus all \
  -e FFMPEG_HARDWARE_ACCEL=1 \
  stremio-server-go
```

#### Docker Compose with GPU
```yaml
services:
  stremio-server-go:
    # ... other config
    devices:
      - "/dev/dri:/dev/dri"  # For VAAPI
    environment:
      - FFMPEG_HARDWARE_ACCEL=1
```

### Checking Hardware Acceleration
```bash
curl http://localhost:11470/hwaccel-profiler
```

This will return information about available hardware acceleration methods.

## Architecture

The Go implementation follows a modular architecture:

- **Main Server**: HTTP/HTTPS server with routing
- **Torrent Manager**: Manages all torrent operations
- **Torrent Engine**: Individual torrent handling with streaming
- **FFmpeg Manager**: Video transcoding and processing
- **API Handlers**: REST API endpoint implementations
- **Middleware**: CORS, authentication, and other middleware

## Development

### Project Structure
```
.
├── main.go          # Main server implementation
├── torrent.go       # Torrent management
├── ffmpeg.go        # FFmpeg integration
├── go.mod           # Go module definition
├── go.sum           # Dependency checksums
├── Dockerfile.go    # Docker build file
└── README.md     # This file
```

### Adding Features
1. Add new handlers in `main.go`
2. Extend torrent functionality in `torrent.go`
3. Update API documentation
4. Add tests for new functionality

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## Support

For issues and questions:
- Create an issue on GitHub
- Check the existing documentation
- Review the API endpoints 