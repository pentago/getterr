# getterr

getterr keeps Sonarr and Radarr hunting for the shows and movies you're still missing, and for better-quality copies of ones you already have.

Sonarr and Radarr only grab new releases as they show up. Anything older, like episodes that aired before you added a show, they won't look for unless you click "Search All" yourself. getterr does that for you on a schedule, the job Huntarr used to do.

## Running it

Add this to your Docker Compose file:

```yaml
services:
  getterr:
    image: ghcr.io/pentago/getterr:latest
    restart: unless-stopped
    environment:
      SONARR_URL: http://sonarr:8989
      SONARR_API_KEY: your-sonarr-api-key
      SONARR_MISSING_PACE: 1w
      SONARR_UNMET_PACE: 1m
      RADARR_URL: http://radarr:7878
      RADARR_API_KEY: your-radarr-api-key
      RADARR_MISSING_PACE: 1w
      RADARR_UNMET_PACE: 1m
    volumes:
      - getterr-data:/data

volumes:
  getterr-data:
```

Then start it with `docker compose up -d getterr`. You'll find the API keys in Sonarr and Radarr under Settings → General.

Use `ghcr.io/pentago/getterr:dev` instead if you want the newest changes before they're released.

## Settings

| Setting | What it does |
|---|---|
| `SONARR_URL`, `RADARR_URL` | Where to reach Sonarr and Radarr. |
| `SONARR_API_KEY`, `RADARR_API_KEY` | Their API keys. |
| `SONARR_MISSING_PACE`, `RADARR_MISSING_PACE` | How often to search for things you don't have yet. |
| `SONARR_UNMET_PACE`, `RADARR_UNMET_PACE` | How often to search for better-quality copies of things you have. |

How often is a number plus a letter:

- `6h`: every 6 hours
- `1d`: every day
- `1w`: every week
- `1m`: every month (30 days)

Leave a setting out to turn that search off. If you only use Sonarr, you can skip all the Radarr settings, and the other way around.

## Good to know

- getterr runs each search once when it first starts, then on your schedule after that.
- It remembers when each search last ran. That's what the `/data` volume is for, so restarting or updating the container won't trigger extra searches.
- If Sonarr or Radarr can't be reached, it tries again every 5 minutes and doesn't lose its schedule.
- Each search goes through your whole Missing or Cutoff Unmet list. On a big library that's a lot of queries, so if your indexers have daily limits, set a Query Limit for each indexer in Prowlarr.
- Check `docker logs getterr` to see what it's doing.
- It works on regular PCs, ARM machines, and Apple silicon Macs running Docker.
