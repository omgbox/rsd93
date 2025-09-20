document.addEventListener('DOMContentLoaded', () => {
  // --- DOM Elements ---
  const inputContainer = document.getElementById('input-container');
  const loader = document.getElementById('loader');
  const mainModal = document.getElementById('main-modal');
  
  const fileListContainer = document.getElementById('file-list-container');
  const playerContainer = document.getElementById('player-container');
  const statusContainer = document.getElementById('status-container');

  const messageInput = document.getElementById('messageInput');
  const sendButton = document.getElementById('sendButton');
  const uploadButtonLabel = document.getElementById('uploadButtonLabel');
  const restartButton = document.getElementById('restart-button');

  const fileList = document.getElementById('file-list');
  const videoPlayer = document.getElementById('video-player');
  const subtitleTrack = document.getElementById('subtitle-track');
  const ffmpegLog = document.getElementById('ffmpeg-log');
  const jassubCanvas = document.getElementById('jassub-canvas');
  const fetchingText = document.getElementById('fetching-text');

  const torrentName = document.getElementById('torrent-name');
  const downloadSpeed = document.getElementById('download-speed');
  const connectedPeers = document.getElementById('connected-peers');
  const percentageCompleted = document.getElementById('percentage-completed');
  const streamingFileSize = document.getElementById('streaming-file-size'); // New: Element for streaming file size

  // --- State ---
  let currentMagnet = '';
  let statusInterval = null;
  let extractionInterval = null;
  let jassubInstance = null;
  let currentPlayingIndex = -1; // New: Store the index of the currently playing file

  // --- Functions ---

  const showLoader = (show) => {
    loader.classList.toggle('hidden', !show);
  };

  const handleSubmission = async () => {
    let input = messageInput.value.trim();
    if (!input) {
      alert('Please enter a magnet link or upload a .torrent file.');
      return;
    }

    showLoader(true);

    const isUrl = input.startsWith('http://') || input.startsWith('https://');

    if (isUrl) {
      try {
        const response = await fetch('/fetch-torrent-url', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
          },
          body: JSON.stringify({ url: input }),
        });

        if (!response.ok) throw new Error(`Failed to fetch remote torrent: ${response.status}`);

        const data = await response.json();
        if (data.magnetLink) {
          input = data.magnetLink;
        } else {
          throw new Error('No magnet link received from server.');
        }
      } catch (error) {
        console.error('Error fetching remote torrent:', error);
        alert(`Failed to fetch remote torrent: ${error.message}`);
        showLoader(false);
        return;
      }
    }

    const magnetRegex = /^magnet:\?xt=urn:btih:([a-fA-F0-9]{40}|[A-Z2-7]{32}).*$/i;
    if (!magnetRegex.test(input)) {
      alert('Invalid magnet link format. Please enter a valid magnet URI or a URL to a .torrent file.');
      showLoader(false);
      return;
    }

    currentMagnet = input;

    try {
      const response = await fetch(`/files?url=${encodeURIComponent(currentMagnet)}`);
      if (!response.ok) throw new Error(`HTTP error! status: ${response.status}`);
      
      const data = await response.json();
      populateFileList(data.Files);
      
      inputContainer.classList.add('hidden');
      fileListContainer.classList.remove('hidden');
      playerContainer.classList.add('hidden');
      mainModal.classList.remove('hidden');

    } catch (error) {
      console.error('Error fetching file list:', error);
      alert('Failed to fetch torrent files. Please check the magnet link and ensure the server is running.');
    } finally {
      showLoader(false);
    }
  };

  const populateFileList = (files) => {
    fileList.innerHTML = '';
    if (!files || files.length === 0) {
      fileList.innerHTML = '<li>No files found in this torrent.</li>';
      return;
    }

    files.forEach((file, index) => {
      const li = document.createElement('li');
      const isMkvVideo = file.path.match(/\.mkv$/i);

      let content = `<span>${file.path}</span><div class="actions">`;
      if (isMkvVideo) {
        content += `<button class="extract-btn" data-index="${index}">Extract</button>`;
      }
      content += `<span class="file-size">${file.size_human}</span></div>`;

      li.innerHTML = content;
      li.querySelector('span').dataset.index = index; // Click on name plays file

      if (file.isSubtitle) {
        li.classList.add('subtitle-item');
        li.dataset.path = file.path;
      }

      fileList.appendChild(li);
    });
  };

  const handleFileClick = (target) => {
    if (!target) return;
    const li = target.closest('li');
    if (!li) return;

    if (target.classList.contains('extract-btn')) {
      extractSubtitles(target.dataset.index);
    } else if (li.classList.contains('subtitle-item')) {
      downloadAndApplySubtitle(li);
    } else {
      playFile(target.dataset.index);
    }
  };

  const disableSubtitles = () => {
    if (videoPlayer.textTracks.length > 0) {
      videoPlayer.textTracks[0].mode = 'disabled';
    }
    subtitleTrack.src = '';
    if (jassubInstance) {
      jassubInstance.destroy();
      jassubInstance = null;
    }
    // Explicitly clear and hide the jassubCanvas
    if (jassubCanvas) {
      const ctx = jassubCanvas.getContext('2d');
      ctx.clearRect(0, 0, jassubCanvas.width, jassubCanvas.height);
      jassubCanvas.style.display = 'none'; // Hide the canvas
    }
    console.log('Subtitles disabled.');
  };

  const downloadAndApplySubtitle = async (li) => {
    const filePath = li.dataset.path;
    if (!filePath) return;

    if (li.classList.contains('active')) {
      li.classList.remove('active');
      disableSubtitles();
      return;
    }

    document.querySelectorAll('#file-list li.active').forEach(el => el.classList.remove('active'));
    li.classList.add('active');
    disableSubtitles(); // Clear any existing subs

    try {
      const response = await fetch(`/download-subtitle?url=${encodeURIComponent(currentMagnet)}&filePath=${encodeURIComponent(filePath)}`);
      if (!response.ok) throw new Error(`Subtitle download failed: ${response.status}`);
      
      const data = await response.json();
      if (data.vttKey) {
        subtitleTrack.src = `/stream-vtt?key=${data.vttKey}`;
        videoPlayer.textTracks[0].mode = 'showing';
      }
    } catch (error) {
      console.error('Error applying subtitle:', error);
      alert('Could not load the selected subtitle.');
      li.classList.remove('active');
    }
  };

  const extractSubtitles = async (index) => {
    if (extractionInterval) clearInterval(extractionInterval);

    // Switch to a "log-only" view
    fileListContainer.classList.add('hidden');
    playerContainer.classList.remove('hidden');
    videoContainer.classList.add('hidden');
    statusContainer.classList.add('hidden');

    ffmpegLog.textContent = 'Starting subtitle extraction...';
    ffmpegLog.style.display = 'block';
    ffmpegLog.classList.remove('hidden');
    fetchingText.textContent = 'fetching subs...';
    fetchingText.style.display = 'block';
    fetchingText.classList.remove('hidden');

    try {
      const response = await fetch(`/extract-subtitles?url=${encodeURIComponent(currentMagnet)}&index=${index}`);
      if (!response.ok) throw new Error(`Extraction request failed: ${response.status}`);
      
      const data = await response.json();
      if (data.logFile && data.subtitleFile) {
        pollExtractionStatus(data.logFile, data.subtitleFile, index);
      } else {
        throw new Error('Invalid response from extraction server.');
      }
    } catch (error) {
      console.error('Error starting subtitle extraction:', error);
      ffmpegLog.textContent = `Error: ${error.message}`;
      fetchingText.classList.add('hidden');
    }
  };

  const pollExtractionStatus = (logFile, subtitleFile, index) => {
    extractionInterval = setInterval(async () => {
      try {
        const response = await fetch(`/subtitles?file=${logFile}`);
        if (!response.ok) {
          ffmpegLog.textContent = `Log file not found or error reading it.`;
          clearInterval(extractionInterval);
          fetchingText.classList.add('hidden');
          return;
        }
        const logText = await response.text();
        if (!logText.trim()) return; // Don't update UI if log is empty

        const lines = logText.trim().split('\n');
        const lastLine = lines[lines.length - 1];

        const progressRegex = new RegExp('size=\\s*(?<size>\\S+)\\s*time=\\s*(?<time>\\S+)\\s*bitrate=\\s*(?<bitrate>\\S+)\\s*speed=\\s*(?<speed>\\S+)');
        const match = lastLine.match(progressRegex);

        if (logText.includes('Extraction finished successfully')) {
          clearInterval(extractionInterval);
          fetchingText.classList.add('hidden');
          fetchingText.style.display = 'none';
          ffmpegLog.style.display = 'none';
          initializeJassub(subtitleFile, index);
        } else if (logText.includes('Extraction failed')) {
          clearInterval(extractionInterval);
          fetchingText.classList.add('hidden');
          ffmpegLog.style.display = 'none';
          ffmpegLog.textContent = `Extraction failed. Last message: ${lastLine}`;
        } else if (match && match.groups) {
          const { size, time, bitrate, speed } = match.groups;
          ffmpegLog.textContent = `Extracting... Size: ${size} | Time: ${time} | Bitrate: ${bitrate} | Speed: ${speed}`;
        } else {
          ffmpegLog.textContent = lastLine; // Fallback for non-progress lines
        }
      } catch (error) {
        console.error('Error polling extraction status:', error);
        ffmpegLog.textContent = `Error polling status: ${error.message}`;
        clearInterval(extractionInterval);
        fetchingText.classList.add('hidden');
      }
    }, 1000);
  };

  const initializeJassub = (subtitleFile, index) => {
    disableSubtitles(); // Clear any old subtitles first
    document.querySelectorAll('#file-list li.active').forEach(el => el.classList.remove('active'));

    const subtitleUrl = `/subtitles?file=${subtitleFile}`;
    const baseUrl = '/jassub_dist/';

    jassubInstance = new JASSUB({
      video: videoPlayer,
      canvas: jassubCanvas,
      subUrl: subtitleUrl,
      workerUrl: baseUrl + 'jassub-worker.js',
      wasmUrl: baseUrl + 'jassub-worker.wasm',
      legacyWasmUrl: baseUrl + 'jassub-worker.wasm.js',
      modernWasmUrl: baseUrl + 'jassub-worker-modern.wasm',
      fallbackFont: 'liberation sans', // as per docs
      availableFonts: {
        'liberation sans': baseUrl + 'default.woff2'
      }
    });

    if (jassubCanvas) {
      jassubCanvas.style.display = 'block'; // Show the canvas when JASSUB is active
    }

    // Add event listeners to the video player for resizing JASSUB
    videoPlayer.addEventListener('resize', () => {
      if (jassubInstance) {
        const videoWidth = videoPlayer.videoWidth;
        const videoHeight = videoPlayer.videoHeight;
        const clientWidth = videoPlayer.clientWidth;
        const clientHeight = videoPlayer.clientHeight;

        let effectiveVideoWidth = clientWidth;
        let effectiveVideoHeight = clientHeight;
        let offsetX = 0;
        let offsetY = 0;

        if (videoWidth && videoHeight && clientWidth && clientHeight) {
          const elementAspectRatio = clientWidth / clientHeight;
          const videoAspectRatio = videoWidth / videoHeight;

          if (elementAspectRatio > videoAspectRatio) { // Container is taller than video
            effectiveVideoWidth = clientHeight * videoAspectRatio;
            offsetX = (clientWidth - effectiveVideoWidth) / 2;
          } else { // Container is wider than video
            effectiveVideoHeight = clientWidth / videoAspectRatio;
            offsetY = (clientHeight - effectiveVideoHeight) / 2;
          }
        }
        jassubInstance.resize(effectiveVideoWidth, effectiveVideoHeight, offsetY, offsetX);
      }
    });

    videoPlayer.addEventListener('loadedmetadata', () => {
      if (jassubInstance) {
        const videoWidth = videoPlayer.videoWidth;
        const videoHeight = videoPlayer.videoHeight;
        const clientWidth = videoPlayer.clientWidth;
        const clientHeight = videoPlayer.clientHeight;

        let effectiveVideoWidth = clientWidth;
        let effectiveVideoHeight = clientHeight;
        let offsetX = 0;
        let offsetY = 0;

        if (videoWidth && videoHeight && clientWidth && clientHeight) {
          const elementAspectRatio = clientWidth / clientHeight;
          const videoAspectRatio = videoWidth / videoHeight;

          if (elementAspectRatio > videoAspectRatio) { // Container is taller than video
            effectiveVideoWidth = clientHeight * videoAspectRatio;
            offsetX = (clientWidth - effectiveVideoWidth) / 2;
          } else { // Container is wider than video
            effectiveVideoHeight = clientWidth / videoAspectRatio;
            offsetY = (clientHeight - effectiveVideoHeight) / 2;
          }
        }
        jassubInstance.resize(effectiveVideoWidth, effectiveVideoHeight, offsetY, offsetX);
      }
    });

    // Show the video and status bar now that we are ready to play
    videoContainer.classList.remove('hidden');
    statusContainer.classList.remove('hidden');

    // Start playing the video file corresponding to the extracted subtitles
    currentPlayingIndex = index; // Set the current playing index
    const streamUrl = `/stream?url=${encodeURIComponent(currentMagnet)}&index=${index}`;
    videoPlayer.src = streamUrl;

    // Add a one-time listener for loadedmetadata
    videoPlayer.addEventListener('loadedmetadata', function handler() {
      videoPlayer.play().catch(e => {
        // Handle potential autoplay policy rejections or other play errors
        console.error("Error attempting to play video:", e);
      });
      videoPlayer.removeEventListener('loadedmetadata', handler); // Remove self
    });
  };

  const playFile = (index) => {
    fileListContainer.classList.add('hidden');
    playerContainer.classList.remove('hidden');
    ffmpegLog.classList.add('hidden');
    ffmpegLog.style.display = 'none';
    fetchingText.classList.add('hidden');
    if (extractionInterval) clearInterval(extractionInterval);

    disableSubtitles();

    currentPlayingIndex = index; // Set the current playing index

    const streamUrl = `/stream?url=${encodeURIComponent(currentMagnet)}&index=${index}`;
    videoPlayer.src = streamUrl;

    // Add a one-time listener for loadedmetadata
    videoPlayer.addEventListener('loadedmetadata', function handler() {
      videoPlayer.play().catch(e => {
        // Handle potential autoplay policy rejections or other play errors
        console.error("Error attempting to play video:", e);
      });
      videoPlayer.removeEventListener('loadedmetadata', handler); // Remove self
    });

    startStatusUpdates();
  };

  const startStatusUpdates = () => {
    if (statusInterval) clearInterval(statusInterval);
    fetchStatus();
    statusInterval = setInterval(fetchStatus, 2000);
  };

  const fetchStatus = async () => {
    if (!currentMagnet) return;
    try {
      const response = await fetch(`/status?url=${encodeURIComponent(currentMagnet)}&index=${currentPlayingIndex}`);
      if (!response.ok) {
        console.warn(`Status fetch failed: ${response.status}`);
        return;
      }
      const status = await response.json();
      torrentName.textContent = status.name || 'Loading...';
      downloadSpeed.textContent = status.downloadSpeedHuman || '0 B/s';
      connectedPeers.textContent = status.connectedPeers || '0';
      percentageCompleted.textContent = status.percentageCompleted.toFixed(2);
      // Update streaming file size
      if (status.streamingFileSizeHuman) {
        streamingFileSize.textContent = ` / ${status.streamingFileSizeHuman}`;
      } else {
        streamingFileSize.textContent = '';
      }
    } catch (error) {
      console.error('Error fetching status:', error);
      if (statusInterval) clearInterval(statusInterval);
    }
  };

  const restartSession = () => {
    videoPlayer.pause();
    videoPlayer.src = '';
    if (statusInterval) clearInterval(statusInterval);
    if (extractionInterval) clearInterval(extractionInterval);

    disableSubtitles();
    document.querySelectorAll('#file-list li.active').forEach(el => el.classList.remove('active'));

    mainModal.classList.add('hidden');
    inputContainer.classList.remove('hidden');
    messageInput.value = '';
    currentMagnet = '';
    ffmpegLog.classList.add('hidden');
    ffmpegLog.textContent = '';
    fetchingText.classList.add('hidden');
  };

  // --- Event Listeners ---
  sendButton.addEventListener('click', handleSubmission);

  messageInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') handleSubmission();
  });

  const fileInput = document.getElementById('file');
  fileInput.addEventListener('change', async (e) => {
    const file = e.target.files[0];
    if (!file) return;

    if (file.type !== 'application/x-bittorrent') {
      alert('Please upload a .torrent file.');
      e.target.value = '';
      return;
    }

    showLoader(true);

    try {
      const reader = new FileReader();
      reader.onload = async (event) => {
        const torrentBuffer = event.target.result;

        const response = await fetch('/upload-torrent', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/x-bittorrent',
            'X-Filename': file.name,
          },
          body: torrentBuffer,
        });

        if (!response.ok) throw new Error(`Torrent upload failed: ${response.status}`);

        const data = await response.json();
        if (data.magnetLink) {
          messageInput.value = data.magnetLink;
          handleSubmission();
        } else {
          throw new Error('No magnet link received from server.');
        }
      };
      reader.readAsArrayBuffer(file);
    } catch (error) {
      console.error('Error uploading torrent file:', error);
      alert(`Failed to upload torrent file: ${error.message}`);
    } finally {
      showLoader(false);
      e.target.value = '';
    }
  });

  restartButton.addEventListener('click', restartSession);

  fileList.addEventListener('click', (e) => {
    handleFileClick(e.target);
  });

  videoPlayer.addEventListener('pause', () => {
    if (statusInterval) clearInterval(statusInterval);
  });
  videoPlayer.addEventListener('play', () => {
    startStatusUpdates();
  });

  // --- Fullscreen Logic ---
  const customFullscreenBtn = document.getElementById('custom-fullscreen-btn');
  const videoContainer = document.getElementById('video-container');

  customFullscreenBtn.addEventListener('click', () => {
    if (document.fullscreenElement) {
      document.exitFullscreen();
    } else {
      videoContainer.requestFullscreen();
    }
  });

  document.addEventListener('fullscreenchange', () => {
    if (document.fullscreenElement) {
      customFullscreenBtn.innerHTML = '&#x26F7;'; // Exit fullscreen symbol
    } else {
      customFullscreenBtn.innerHTML = '&#x26F6;'; // Enter fullscreen symbol
    }
    // Resize JASSUB canvas after a short delay to allow for screen mode transition
    if (jassubInstance) {
      setTimeout(() => jassubInstance.resize(), 100);
    }
  });
});