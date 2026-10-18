{ pkgs, lib, config, ... }:

{
  packages = with pkgs; [
    git
    ffmpeg
    yt-dlp
    golangci-lint
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

  enterTest = ''
    set -e
    go test ./...
    golangci-lint run
  '';

  processes = lib.optionalAttrs (!config.devenv.isTesting) {
    noctune.exec = ''
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
          -e CACHE_MAX_BYTES \
          -e AVATAR_CACHE_MAX_BYTES \
          -e MAX_YTDLP_PROCESSES \
          -p 8080:8080 \
          noctune:latest
    '';
  };
}
