{ pkgs, ... }:

{
  packages = with pkgs; [
    git
    ffmpeg
    yt-dlp
  ];

  languages.go = {
    enable = true;
  };

  enterShell = ''
    echo "noctune dev shell"
    go version
    ffmpeg -version | head -n1
    yt-dlp --version
  '';

  processes.noctune.exec = ''
    docker build -t noctune:latest .
    secretspec run --provider infisical --profile default -- \
      docker run --rm \
        -e DISCORD_BOT_TOKEN \
        -e DISCORD_TEST_GUILD_ID \
        -e SPOTIFY_CLIENT_ID \
        -e SPOTIFY_CLIENT_SECRET \
        -e WEB_LISTEN_ADDR \
        -e WEB_AUTH_TOKEN \
        -e WEB_PUBLIC_URL \
        -e DISCORD_CLIENT_ID \
        -e DISCORD_CLIENT_SECRET \
        -e DISCORD_OAUTH_REDIRECT_URL \
        -e SESSION_SECRET \
        -e DEFAULT_VOLUME \
        -e MAX_QUEUE_SIZE \
        -e IDLE_DISCONNECT_SECONDS \
        -e LOG_LEVEL \
        -e FFMPEG_PATH \
        -e YTDLP_PATH \
        -e CACHE_DIR \
        -p 8080:8080 \
        noctune:latest
  '';
}
