FROM mwader/static-ffmpeg:7.0-1 AS ffmpeg

FROM python:3.15-slim
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffprobe /usr/local/bin/ffprobe
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY sidecar ./sidecar
ENTRYPOINT ["uvicorn", "sidecar.app:create_app", "--factory", "--host", "0.0.0.0", "--port", "8082"]
