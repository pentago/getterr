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
      SONARR_MISSING_PACE: 1h
      SONARR_MISSING_BATCH_SIZE: 10
      SONARR_UNMET_PACE: 1w
      RADARR_URL: http://radarr:7878
      RADARR_API_KEY: your-radarr-api-key
      RADARR_MISSING_PACE: 1h
      RADARR_MISSING_BATCH_SIZE: 5
      RADARR_UNMET_PACE: 1w
      BAZARR_URL: http://bazarr:6767
      BAZARR_API_KEY: your-bazarr-api-key
      BAZARR_SERIES_PACE: 30m
      BAZARR_SERIES_BATCH_SIZE: 5
      BAZARR_MOVIES_PACE: 1h
      BAZARR_MOVIES_BATCH_SIZE: 5
    volumes:
      - ./config/getterr:/data
```

Create the `config/getterr` folder yourself first (`mkdir -p config/getterr`), so it belongs to the user getterr runs as (`user:` above). Otherwise Docker creates it owned by root, and getterr stops right away because it can't save anything there. Then start it with `docker compose up -d getterr`. You'll find the API keys in Sonarr, Radarr and Bazarr under Settings → General.

Use `ghcr.io/pentago/getterr:dev` instead if you want the newest changes before they're released.

## Settings

| Setting | What it does |
|---|---|
| `SONARR_URL`, `RADARR_URL`, `BAZARR_URL` | Where to reach Sonarr, Radarr and Bazarr. |
| `SONARR_API_KEY`, `RADARR_API_KEY`, `BAZARR_API_KEY` | Their API keys. |
| `SONARR_MISSING_PACE`, `RADARR_MISSING_PACE` | How often to search for things you don't have yet. |
| `SONARR_UNMET_PACE`, `RADARR_UNMET_PACE` | How often to search for better-quality copies of things you have. |
| `BAZARR_SERIES_PACE`, `BAZARR_MOVIES_PACE` | How often to search for missing episode and movie subtitles. |
| `SONARR_MISSING_BATCH_SIZE`, `SONARR_UNMET_BATCH_SIZE`, `RADARR_MISSING_BATCH_SIZE`, `RADARR_UNMET_BATCH_SIZE`, `BAZARR_SERIES_BATCH_SIZE`, `BAZARR_MOVIES_BATCH_SIZE` | Optional. Search only this many episodes or movies each time instead of the whole list. |

How often is a number plus a letter:

- `10m`: every 10 minutes
- `6h`: every 6 hours
- `1d`: every day
- `1w`: every week
- `1M`: every 30 days

Small `m` is minutes and capital `M` is months. To spare your indexers, a search over the whole list can run at most once an hour, and a batch at most every 10 minutes; getterr refuses to start with a shorter pace. In version 0.1.1 and earlier, small `m` meant months, so if you're upgrading, change settings like `1m` to `1M`.

Leave a setting out to turn that search off. If you only use Sonarr, you can skip all the Radarr and Bazarr settings, and so on.

To work through a big list a little at a time, give a search a batch size and a short pace. In the example above, `SONARR_MISSING_BATCH_SIZE: 10` with `SONARR_MISSING_PACE: 1h` searches 10 missing episodes every hour, while `SONARR_UNMET_PACE: 1w` has no batch size and searches the whole Cutoff Unmet list once a week. Bazarr batches search each episode or movie in every language it's missing.

Each batch goes through the list in order, picks up where the last one left off, and starts over at the end, so everything gets its turn.

In version 0.1.2 and earlier, `SONARR_BATCH_SIZE` and `RADARR_BATCH_SIZE` covered both searches of the app. getterr now refuses to start while they're set; use the `_MISSING_BATCH_SIZE` and `_UNMET_BATCH_SIZE` settings instead.

When getterr handles a Bazarr search, set that search to "Never" in Bazarr under Settings → Scheduler (Search for Missing Series Subtitles, Search for Missing Movies Subtitles). Otherwise Bazarr still runs its own full search too. Bazarr's subtitle upgrades aren't covered by getterr; they only look at subtitles from the last few weeks, so Bazarr's own schedule is fine for them.

## Good to know

- getterr runs each search once when it first starts, then on your schedule after that.
- It remembers when each search last ran. That's what the `/data` volume is for, so restarting or updating the container won't trigger extra searches.
- When it starts, getterr checks that it can talk to Sonarr, Radarr and Bazarr. If one rejects your API key, or the address points to the wrong app, getterr stops right away and the log says what to fix.
- If an app can't be reached, at startup or later, getterr tries again every 5 minutes and doesn't lose its schedule. A mistyped address looks the same as an app that's still starting, so if the log keeps saying `cannot reach`, check the address.
- Without a batch size, each search goes through your whole Missing or Cutoff Unmet list. On a big library that's a lot of queries, so either set a batch size or, if your indexers have daily limits, set a Query Limit for each indexer in Prowlarr.
- Check `docker logs getterr` to see what it's doing.
- It works on regular PCs, ARM machines, and Apple silicon Macs running Docker.
