"""yt-fifo: a YouTube queue that streams audio into Snapcast, as a media_player.

Configured as a YAML media_player platform:

    media_player:
      - platform: yt_fifo
        host: 192.168.1.181
        port: 8888
        name: Home audio YouTube
"""

DOMAIN = "yt_fifo"
