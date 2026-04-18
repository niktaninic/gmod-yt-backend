FROM node:20-slim

# curl_cffi is here for yt-dlp impersonate stuff. build deps get tossed after install
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 \
    python3-pip \
    python3-dev \
    gcc \
    g++ \
    make \
    libcurl4-openssl-dev \
    libssl-dev \
    ffmpeg \
    curl \
    ca-certificates \
    && pip3 install --break-system-packages "yt-dlp[default]" curl_cffi \
    && apt-get purge -y python3-dev gcc g++ make libcurl4-openssl-dev libssl-dev \
    && apt-get autoremove -y \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/local/bin/node /usr/bin/node

WORKDIR /app

COPY package.json package-lock.json* ./
RUN npm ci --omit=dev 2>/dev/null || npm install --omit=dev

COPY src/ ./src/
RUN mkdir -p /app/data /app/cache /app/cookies

EXPOSE 9999

CMD ["node", "src/index.js"]
