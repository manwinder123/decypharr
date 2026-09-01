#!/usr/bin/env python3
"""
bridge_one.py — one-off TorBox/AllDebrid -> Real-Debrid bridge via DMM's hosted webseed fleet

What it does (plain English):
  1. Takes a torrent hash that Real-Debrid blocked as `451 infringing_file` but TorBox/AllDebrid has
  2. Asks DMM's servers (debrid01/debrid02) to re-build that torrent with de-infringed filenames
     (WEB-DL -> WEB.DL, BDRip -> BD-Rip, etc) and host it as a webseed
  3. DMM's server adds the *new* webseed torrent to YOUR Real-Debrid account using YOUR RD key
  4. Real-Debrid fetches the bytes from DMM's server over HTTP. Once done, the file is cached in RD
     and you can mount/stream it via decypharr, or let materialize_rd.py copy it to das_pool

Why this script and not automatic?
  - Your decypharr is configured with bridge.use_dmm = OFF by default (intentionally).
  - Automatic bridging would send your RD+TB keys to a third party on every 451 without you knowing.
  - This script is *opt-in* per hash: you decide which of your 119 rd_infringing items are worth bridging,
    and you see exactly what is sent.

Usage:
  python3 bridge_one.py <40-char-hash> [--imdb tt1234567] [--size 1234567890] [--dry-run] [--no-poll]
  python3 bridge_one.py --hash a1b2c3... --imdb tt1234567 --dry-run
  python3 bridge_one.py a1b2c3... --poll-timeout 600

Keys:
  By default reads your RD/TB/AD keys from quickstack/config/decypharr/config.json (the same file
  decypharr uses). Override with --rd-key / --tb-key / --ad-key if you want.

Example for your current 119:
  python3 bridge_one.py --dry-run 4a3b...  # see what would be sent
  python3 bridge_one.py 4a3b... --imdb tt0944947 --poll  # bridge Game of Thrones S01

Polling:
  After submit, the script polls DMM every 5s up to --poll-timeout (default 1800s = 30min) until
  status is "completed" or "failed". You can Ctrl-C and check later with --no-poll + manual GET,
  or just let cli_debrid's next sync pick up the new RD torrent.

DMM API docs (reverse-engineered from debrid-media-manager repo):
  POST https://debridmediamanager.com/api/debrid-uploader/jobs
    { hash, imdbId, rdKey, tbKey?, adKey?, sizeBytes? } -> { id } or { duplicate, rewrittenHash }

You need an internet connection and valid RD + at least one of TB/AD. No VPN needed — DMM's server
does the heavy lifting. Your Plex never needs to mount the webseed; it just needs the final RD torrent
to be cached so decypharr can copy it to das_pool via your existing materialize_rd.py.

If you prefer not to trust DMM, don't use this — wait for TorBox freeze to lift (Aug 24) and your
existing requeue_rd_infringing.py + TorBox fallback will handle those 119 without any bridging.
"""
import argparse
import json
import sys
import time
from pathlib import Path

try:
    import requests
except ImportError:
    print("Missing 'requests' — run: pip install requests", file=sys.stderr)
    sys.exit(1)

DEFAULT_DMM = "https://debridmediamanager.com"
CONFIG_CANDIDATES = [
    Path("/home/mdoodle/quickstack/config/decypharr/config.json"),
    Path("/home/mdoodle/quickstack/config/decypharr/config.json"),  # same, for clarity
]

def load_keys_from_config(path: Path):
    """Read RD/TB/AD keys from decypharr config.json"""
    try:
        data = json.loads(path.read_text())
    except Exception as e:
        return {}, f"read {path}: {e}"
    keys = {}
    for d in data.get("debrids", []):
        provider = d.get("provider", "").lower()
        api_key = d.get("api_key", "")
        if not api_key:
            continue
        if provider == "realdebrid" or provider == "real-debrid":
            keys["rd"] = api_key
        elif provider == "torbox":
            keys["tb"] = api_key
        elif provider == "alldebrid":
            keys["ad"] = api_key
    return keys, None

def find_config_keys():
    for p in CONFIG_CANDIDATES:
        if p.exists():
            keys, err = load_keys_from_config(p)
            if keys:
                return keys, p, None
            if err:
                return {}, p, err
    return {}, None, "no config found"

def add_rewritten_to_rd(rd_key: str, rewritten_hash: str):
    """Add the *rewritten* hash directly to RD — it's already cached, so this is instant."""
    try:
        r = requests.post(
            "https://api.real-debrid.com/rest/1.0/torrents/addMagnet",
            headers={"Authorization": f"Bearer {rd_key}"},
            data={"magnet": f"magnet:?xt=urn:btih:{rewritten_hash}"},
            timeout=15,
        )
        if r.status_code not in (200, 201):
            return False, f"RD addMagnet {r.status_code}: {r.text[:300]}"
        j = r.json()
        tid = j.get("id")
        if not tid:
            return False, f"no id in {j}"
        # select all files
        r2 = requests.post(
            f"https://api.real-debrid.com/rest/1.0/torrents/selectFiles/{tid}",
            headers={"Authorization": f"Bearer {rd_key}"},
            data={"files": "all"},
            timeout=15,
        )
        if r2.status_code not in (200, 201, 204):
            return False, f"selectFiles {r2.status_code}: {r2.text[:300]}"
        return True, tid
    except Exception as e:
        return False, str(e)

def main():
    ap = argparse.ArgumentParser(description="Bridge one TorBox/AD hash to RD via DMM's webseed fleet (opt-in, one at a time)")
    ap.add_argument("hash", nargs="?", help="40-char hex infohash (lower or upper)")
    ap.add_argument("--hash", dest="hash_opt", help="same as positional")
    ap.add_argument("--imdb", default=None, help="IMDb ID like tt1234567 (required by DMM; placeholder tt0000000 used if omitted)")
    ap.add_argument("--size", type=int, default=None, help="torrent size bytes (helps DMM route to uncapped host; optional)")
    ap.add_argument("--rd-key", default=None, help="Real-Debrid API key (default: from decypharr config)")
    ap.add_argument("--tb-key", default=None, help="TorBox API key (default: from decypharr config)")
    ap.add_argument("--ad-key", default=None, help="AllDebrid API key (default: from decypharr config)")
    ap.add_argument("--dmm", default=DEFAULT_DMM, help=f"DMM base URL (default {DEFAULT_DMM})")
    ap.add_argument("--config", default=None, help="path to decypharr config.json (default auto-find)")
    ap.add_argument("--dry-run", action="store_true", help="print what would be sent, don't POST")
    ap.add_argument("--no-poll", action="store_true", help="submit but don't poll for completion")
    ap.add_argument("--poll-timeout", type=int, default=1800, help="seconds to poll for completion (default 1800)")
    ap.add_argument("--poll-interval", type=int, default=5, help="seconds between polls (default 5)")
    args = ap.parse_args()

    h = (args.hash or args.hash_opt or "").strip().lower()
    if not h:
        ap.print_help()
        print("\nERROR: provide a 40-char hash as positional arg or --hash", file=sys.stderr)
        sys.exit(2)
    if len(h) != 40 or any(c not in "0123456789abcdef" for c in h):
        print(f"ERROR: hash must be 40 hex chars, got {h!r} len {len(h)}", file=sys.stderr)
        sys.exit(2)

    # Load keys from config if not provided
    rd_key = args.rd_key
    tb_key = args.tb_key
    ad_key = args.ad_key
    config_path = Path(args.config) if args.config else None

    if not rd_key or not tb_key:
        # try config
        if config_path:
            keys, err = load_keys_from_config(config_path)
            src = str(config_path)
        else:
            keys, src, err = find_config_keys()
        if err:
            print(f"WARN: could not load keys from config: {err}", file=sys.stderr)
        if not rd_key:
            rd_key = keys.get("rd")
        if not tb_key:
            tb_key = keys.get("tb")
        if not ad_key:
            ad_key = keys.get("ad")
        if rd_key or tb_key or ad_key:
            print(f"Loaded keys from {src}: RD={'yes' if rd_key else 'no'} TB={'yes' if tb_key else 'no'} AD={'yes' if ad_key else 'no'}")
        else:
            print(f"WARN: no keys found in {src}, you must pass --rd-key/--tb-key", file=sys.stderr)

    if not rd_key:
        print("ERROR: Real-Debrid key required (--rd-key or decypharr config)", file=sys.stderr)
        sys.exit(2)
    if not tb_key and not ad_key:
        print("ERROR: at least one source key required (--tb-key or --ad-key or decypharr config)", file=sys.stderr)
        sys.exit(2)

    imdb = args.imdb or "tt0000000"
    if not imdb.startswith("tt"):
        print(f"WARN: imdb {imdb!r} should look like tt1234567, using anyway", file=sys.stderr)

    payload = {
        "hash": h,
        "imdbId": imdb,
        "rdKey": rd_key,
        "sizeBytes": args.size,
    }
    if tb_key:
        payload["tbKey"] = tb_key
    if ad_key:
        payload["adKey"] = ad_key
    # remove None size
    if args.size is None:
        payload.pop("sizeBytes", None)

    print(f"\n=== Bridge request ===")
    print(f"  hash: {h}")
    print(f"  imdb: {imdb}")
    if args.size:
        print(f"  size: {args.size} ({args.size/1024/1024/1024:.2f} GB)")
    print(f"  DMM:  {args.dmm}/api/debrid-uploader/jobs")
    print(f"  RD key: ...{rd_key[-6:]}  TB key: ...{tb_key[-6:] if tb_key else 'none'}  AD key: ...{ad_key[-6:] if ad_key else 'none'}")
    if args.dry_run:
        print("\n--dry-run: would POST payload (keys redacted):")
        redacted = {k: ("..."+v[-6:] if k.lower().endswith("key") else v) for k, v in payload.items()}
        print(json.dumps(redacted, indent=2))
        print("\nDry run done. Remove --dry-run to actually submit.")
        sys.exit(0)

    # Submit
    url = args.dmm.rstrip("/") + "/api/debrid-uploader/jobs"
    print(f"\nPOST {url} ...")
    try:
        r = requests.post(url, json=payload, timeout=30)
    except Exception as e:
        print(f"ERROR: POST failed: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        data = r.json()
    except Exception:
        data = {"raw": r.text[:2000]}

    print(f"HTTP {r.status_code}")
    print(json.dumps(data, indent=2))

    if r.status_code < 200 or r.status_code >= 300:
        err = data.get("error") or data.get("raw") or r.text[:500]
        print(f"\nFailed: {err}", file=sys.stderr)
        sys.exit(1)

    # Handle duplicate case — already bridged by someone else
    if "duplicate" in data:
        dup = data.get("duplicate")
        rewritten = data.get("rewrittenHash") or data.get("rewritten_hash")
        job_id = data.get("jobId") or data.get("job_id") or ""
        added = data.get("addedToRd")
        print(f"\nDuplicate: {dup}  rewrittenHash={rewritten}  addedToRd={added}  jobId={job_id}")
        if dup == "completed" and rewritten:
            print(f"\nThis hash was already bridged by someone else and the *new* hash {rewritten} is already RD-cached.")
            print(f"Trying to add rewritten hash directly to YOUR RD (instant, no webseed fetch)...")
            ok, info = add_rewritten_to_rd(rd_key, rewritten)
            if ok:
                print(f"  -> Added to your RD as torrent {info} (selectFiles: all). It should appear in decypharr's next sync (2m) and then be copyable to das_pool.")
            else:
                print(f"  -> Direct add failed: {info}")
                print(f"     You can still add it manually: magnet:?xt=urn:btih:{rewritten}")
            if added:
                print(f"  DMM says it also added it for you (addedToRd=true), so you may already see it in RD.")
        else:
            print(f"  Job {job_id} is still {dup}. Poll it with: python3 bridge_one.py --hash {h} --poll (or wait for DMM's Transfers page)")
        sys.exit(0)

    job_id = data.get("id") or data.get("jobId") or data.get("job_id")
    if not job_id:
        print("No job id in response, cannot poll", file=sys.stderr)
        sys.exit(0)

    print(f"\nCreated job {job_id}")
    if args.no_poll:
        print(f"Skipping poll (--no-poll). Poll manually: curl {args.dmm}/api/debrid-uploader/jobs/{job_id}")
        sys.exit(0)

    # Poll
    print(f"Polling every {args.poll_interval}s up to {args.poll_timeout}s (Ctrl-C to stop, job keeps running on DMM)...")
    deadline = time.time() + args.poll_timeout
    while time.time() < deadline:
        time.sleep(args.poll_interval)
        try:
            pr = requests.get(f"{args.dmm.rstrip('/')}/api/debrid-uploader/jobs/{job_id}", timeout=15)
            pj = pr.json()
        except Exception as e:
            print(f"  poll error: {e}")
            continue
        status = pj.get("status") or pj.get("Status") or "unknown"
        rh = pj.get("rewrittenHash") or pj.get("rewritten_hash")
        errm = pj.get("error")
        print(f"  [{time.strftime('%H:%M:%S')}] status={status} rewritten={rh} {errm or ''}")
        if status in ("completed", "failed"):
            if status == "completed" and rh:
                print(f"\nCompleted! Rewritten hash {rh} is now RD-cached.")
                print(f"DMM will also register it so it shows up as cached for everyone.")
                # Try instant add to caller's RD as well (DMM's dedup path does this too for duplicates,
                # but for a fresh job the caller is the owner so their RD already has it via webseed.
                # Still, ensure it's selected.)
                ok, info = add_rewritten_to_rd(rd_key, rh)
                if ok:
                    print(f"  -> Verified/added to your RD as {info}. Wait ~2m for decypharr sync, then check /mnt/zurg/__all__ or run materialize_rd.py")
                else:
                    print(f"  -> Note: direct add check: {info} (may already be there via webseed)")
            else:
                print(f"\nFailed: {errm or pj}")
            break
    else:
        print(f"\nPoll timeout after {args.poll_timeout}s. Job {job_id} still running on DMM.")
        print(f"Check later: GET {args.dmm}/api/debrid-uploader/jobs/{job_id}  or run this script again with the same hash (it will return duplicate)")

if __name__ == "__main__":
    main()
