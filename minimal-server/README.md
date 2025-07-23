# Minimal Stremio Server

A minimal Go server that implements only the essential endpoints needed for Stremio web player compatibility.

## Configuration

The server serves a single hardcoded file. To change the content being served, modify these constants in `main.go`:

```go
const (
    INFO_HASH  = "eadcdc5fcebef5c1304366758369d918df946fe7"
    FILE_INDEX = 0
    FILE_PATH  = "/home/vscode/.stremio-server/stremio-cache/eadcdc5fcebef5c1304366758369d918df946fe7/0"
)
```

## Endpoints

This server implements the following minimal endpoints:

1. **Stats endpoint**: `/{infoHash}/{fileIndex}/stats.json`
   - Returns torrent/file statistics
   - Example: `/febf028a47c4847af1c7bd8b68e2eec2a4b7bcf5/0/stats.json`

2. **Probe endpoint**: `/probe`
   - Returns media file information for probing
   - Used by the web player to understand media format

3. **HLS Audio Init**: `/hlsv2/{infoHash}/{fileIndex}/audio0/init.mp4`
   - Returns HLS audio initialization segment
   - Essential for HLS audio streaming

4. **HLS Video Init**: `/hlsv2/{infoHash}/{fileIndex}/video/init.mp4`
   - Returns HLS video initialization segment
   - Essential for HLS video streaming

5. **HLS Master Playlist**: `/hlsv2/{infoHash}/{fileIndex}/master.m3u8`
   - Returns HLS master playlist with video, audio, and subtitle tracks
   - Essential for HLS streaming with multiple tracks

6. **Casting endpoint**: `/casting`
   - Returns casting device information
   - Used for Chromecast and other casting functionality

7. **Health check**: `/health`
   - Simple health check endpoint

## Running the Server

```bash
cd minimal-server
go mod tidy
go run main.go
```

The server will start on port 11470 by default.

## Testing

You can test the endpoints with curl:

```bash
# Test stats endpoint
curl http://localhost:11470/eadcdc5fcebef5c1304366758369d918df946fe7/0/stats.json

# Test probe endpoint
curl http://localhost:11470/probe

# Test HLS audio init endpoint
curl http://localhost:11470/hlsv2/eadcdc5fcebef5c1304366758369d918df946fe7/0/audio0/init.mp4

# Test HLS video init endpoint
curl http://localhost:11470/hlsv2/eadcdc5fcebef5c1304366758369d918df946fe7/0/video0/init.mp4

# Test HLS master playlist endpoint
curl "http://localhost:11470/hlsv2/eadcdc5fcebef5c1304366758369d918df946fe7/0/master.m3u8?mediaURL=http%3A%2F%2F127.0.0.1%3A11470%2Ffebf028a47c4847af1c7bd8b68e2eec2a4b7bcf5%2F0&videoCodecs=h264&videoCodecs=h265&videoCodecs=hevc&audioCodecs=aac&audioCodecs=mp3&audioCodecs=opus&maxAudioChannels=2"

# Test casting endpoint
curl http://localhost:11470/casting

# Test health endpoint
curl http://localhost:11470/health
```

## Features

- **CORS enabled**: All endpoints support cross-origin requests
- **Mock responses**: Returns realistic but mock data for testing
- **Minimal dependencies**: Only uses gorilla/mux for routing
- **Simple structure**: Easy to understand and extend

## Purpose

This minimal server is designed to provide just enough functionality to make the Stremio web player work for basic testing and development purposes. 