# RSD9.3 - Web-based Torrent Streamer

RSD9.3 is a web-based torrent client designed for seamless streaming, file management, and status monitoring of torrents directly through an HTTP API.

## Features

-   **Direct Streaming:** Stream torrent content directly in your web browser.
-   **Flexible Input:** Supports adding torrents via magnet links and remote `.torrent` file URLs.
-   **Subtitle Support:** Extract and stream subtitles (SRT to VTT, ASS) for an enhanced viewing experience.
-   **Persistent Metadata:** Stores torrent metadata persistently using LotusDB for quick retrieval.
-   **Efficient Caching:** Utilizes an LRU (Least Recently Used) cache for frequently accessed torrents, optimizing performance and resource usage.
-   **Automated Cleanup:** Automatically cleans up inactive torrents to manage resources.

## Getting Started

### Prerequisites

-   Go (Golang) installed on your system.
-   `ffmpeg` installed and available in your system's PATH for subtitle extraction functionality.

### Running the Application

1.  Navigate to the project's root directory in your terminal.
2.  Run the application using the Go command:

    ```bash
    go run .
    ```

3.  The server will start, typically on port `3000` (or as configured). Open your web browser and navigate to `http://localhost:3000` to access the interface.

## API Endpoints

The application exposes several HTTP API endpoints for interacting with torrents:

-   **`/stream`**: Stream torrent files directly to your browser.
    -   `GET /stream?url=<magnet_link>&index=<file_index>`
-   **`/files`**: List all files contained within a torrent.
    -   `GET /files?url=<magnet_link>`
-   **`/metadata`**: Retrieve detailed metadata about a torrent.
    -   `GET /metadata?url=<magnet_link>`
-   **`/status`**: Get the current download status of a torrent, including progress, speed, and connected peers.
    -   `GET /status?url=<magnet_link>&index=<file_index>`
-   **`/download-subtitle`**: Download an SRT subtitle file from a torrent and convert it to VTT format.
    -   `GET /download-subtitle?url=<magnet_link>&filePath=<subtitle_file_path>`
-   **`/stream-vtt`**: Stream a converted VTT subtitle file.
    -   `GET /stream-vtt?key=<vtt_filename_key>`
-   **`/extract-subtitles`**: Extract embedded subtitles from video files within a torrent using `ffmpeg`.
    -   `GET /extract-subtitles?url=<magnet_link>&index=<file_index>`
-   **`/subtitles`**: Serve extracted subtitle files (e.g., ASS, log files).
    -   `GET /subtitles?file=<filename>`
-   **`/fetch-torrent-url`**: Add a torrent by providing a URL to a `.torrent` file.
    -   `POST /fetch-torrent-url` with JSON body `{"url": "http://example.com/path/to/torrent.torrent"}`
-   **`/restart`**: Restart the application server.
    -   `GET /restart`



## Build Information

This version is based on a confirmed working state. Build: 7342