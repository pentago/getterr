# getterr

getterr keeps Sonarr and Radarr hunting for the shows and movies you're still missing, and for better-quality copies of ones you already have. It can also keep Bazarr looking for missing subtitles.

Sonarr and Radarr only grab new releases as they show up. Anything older, like episodes that aired before you added a show, they won't look for unless you click "Search All" yourself. getterr does that for you on a schedule, the job now defunct Huntarr used to do.

Bazarr does search on its own schedule, but it searches your whole list at once, and on a big library the subtitle sites start refusing or timing out. getterr can search a few episodes or movies at a time instead, spread over the day.

## Running it

Add this to your Docker Compose file:

```yaml
services:
  getterr:
    image: ghcr.io/pentago/getterr:latest
    user: 1000:1000
    restart: unless-stopped
    environment:
      SONARR_URL: http://sonarr:8989
      SONARR_API_KEY: your-sonarr-api-key
      SONARR_MISSING_PACE: 1w
      SONARR_UNMET_PACE: 1M
      SONARR_BATCH_SIZE: 10
      RADARR_URL: http://radarr:7878
      RADARR_API_KEY: your-radarr-api-key
      RADARR_MISSING_PACE: 1w
      RADARR_UNMET_PACE: 1M
      RADARR_BATCH_SIZE: 5
      BAZARR_URL: http://bazarr:6767
      BAZARR_API_KEY: your-bazarr-api-key
      BAZARR_SERIES_PACE: 30m
      BAZARR_SERIES_BATCH_SIZE: 5
      BAZARR_MOVIES_PACE: 1h
      BAZARR_MOVIES_BATCH_SIZE: 5
    volumes:
      - ./config/getterr:/data
```

Then start it with `docker compose up -d getterr`. You'll find the API keys in Sonarr, Radarr and Bazarr under Settings → General.

Use `ghcr.io/pentago/getterr:dev` instead if you want the newest changes before they're released.

## Settings

| Setting | What it does |
|---|---|
| `SONARR_URL`, `RADARR_URL`, `BAZARR_URL` | Where to reach Sonarr, Radarr and Bazarr. |
| `SONARR_API_KEY`, `RADARR_API_KEY`, `BAZARR_API_KEY` | Their API keys. |
| `SONARR_MISSING_PACE`, `RADARR_MISSING_PACE` | How often to search for things you don't have yet. |
| `SONARR_UNMET_PACE`, `RADARR_UNMET_PACE` | How often to search for better-quality copies of things you have. |
| `BAZARR_SERIES_PACE`, `BAZARR_MOVIES_PACE` | How often to search for missing episode and movie subtitles. |
| `SONARR_BATCH_SIZE`, `RADARR_BATCH_SIZE`, `BAZARR_SERIES_BATCH_SIZE`, `BAZARR_MOVIES_BATCH_SIZE` | Optional. Search only this many episodes or movies each time instead of the whole list. |

How often is a number plus a letter:

- `10m`: every 10 minutes
- `6h`: every 6 hours
- `1d`: every day
- `1w`: every week
- `1M`: every 30 days

Small `m` is minutes and capital `M` is months. In version 0.1.1 and earlier, small `m` meant months, so if you're upgrading, change settings like `1m` to `1M`, or getterr will search every minute.

Leave a setting out to turn that search off. If you only use Sonarr, you can skip all the Radarr and Bazarr settings, and so on.

To work through a big list a little at a time, pair a batch size with a short pace. For example, `RADARR_BATCH_SIZE: 5` with `RADARR_MISSING_PACE: 1h` searches 5 movies every hour, and `SONARR_BATCH_SIZE: 10` with `SONARR_MISSING_PACE: 1h` searches 10 episodes every hour. Each batch picks the ones that have never been searched first, then the ones searched longest ago, so over time everything gets its turn. The batch size applies to both the Missing and Cutoff Unmet searches of that app.

Bazarr batches work the same way, but each search has its own batch size: `BAZARR_SERIES_BATCH_SIZE: 10` with `BAZARR_SERIES_PACE: 1h` searches subtitles for 10 episodes every hour, and `BAZARR_MOVIES_BATCH_SIZE: 5` with `BAZARR_MOVIES_PACE: 1h` does 5 movies every hour, each in every language it's missing. Bazarr doesn't record when it last searched something, so getterr goes through the list in order, picks up where it left off, and starts over at the end.

When getterr handles a Bazarr search, set that search to "Never" in Bazarr under Settings → Scheduler (Search for Missing Series Subtitles, Search for Missing Movies Subtitles). Otherwise Bazarr still runs its own full search too. Bazarr's subtitle upgrades aren't covered by getterr; they only look at subtitles from the last few weeks, so Bazarr's own schedule is fine for them.

## Good to know

- getterr runs each search once when it first starts, then on your schedule after that.
- It remembers when each search last ran. That's what the `/data` volume is for, so restarting or updating the container won't trigger extra searches.
- When it starts, getterr checks that it can talk to Sonarr, Radarr and Bazarr. If one rejects your API key, or the address points to the wrong app, getterr stops right away and the log says what to fix.
- If an app can't be reached, at startup or later, getterr tries again every 5 minutes and doesn't lose its schedule. A mistyped address looks the same as an app that's still starting, so if the log keeps saying `cannot reach`, check the address.
- Without a batch size, each search goes through your whole Missing or Cutoff Unmet list. On a big library that's a lot of queries, so either set a batch size or, if your indexers have daily limits, set a Query Limit for each indexer in Prowlarr.
- Check `docker logs getterr` to see what it's doing.
- It works on regular PCs, ARM machines, and Apple silicon Macs running Docker.
