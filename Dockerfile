# Python slim with ffmpeg
FROM python:3.11-slim

# Install ffmpeg (includes ffprobe)
RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg && \
    rm -rf /var/lib/apt/lists/*

# App files
WORKDIR /app
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

# Copy server
COPY yt_fifo_server.py /app/yt_fifo_server.py

# Non-root optional (comment out if you need root)
# RUN useradd -m app && chown -R app:app /app
# USER app

# HTTP port
EXPOSE 8080

# Healthcheck hits /health
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD \
  wget -qO- http://127.0.0.1:8080/health || exit 1

# Use tini via --init at runtime for clean signal handling
ENV PYTHONUNBUFFERED=1

CMD ["python", "yt_fifo_server.py"]

