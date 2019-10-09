// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the AGPLv3,
// which can be found in the LICENSE file or at gnu.org/licenses/agpl.html

package cron

import (
	"context"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/mssola/user_agent"
	"github.com/pkg/errors"
	"zgo.at/zdb"
	"zgo.at/zdb/bulk"
	"zgo.at/zhttp/ctxkey"

	"zgo.at/goatcounter"
	"zgo.at/goatcounter/cfg"
)

type bstat struct {
	Browser   string    `db:"browser"`
	Count     int       `db:"count"`
	CreatedAt time.Time `db:"created_at"`
}

func updateBrowserStats(ctx context.Context, site goatcounter.Site) error {
	ctx = context.WithValue(ctx, ctxkey.Site, &site)
	db := zdb.MustGet(ctx)

	// Select everything since last update.
	var last string
	if site.LastStat == nil {
		last = "1970-01-01"
	} else {
		last = site.LastStat.Format("2006-01-02")
	}

	var query string
	if cfg.PgSQL {
		query = `
			select
				browser,
				count(browser) as count,
				cast(substr(cast(created_at as varchar), 0, 14) || ':00:00' as timestamp) as created_at
			from hits
			where
				site=$1 and
				created_at>=$2
			group by browser, substr(cast(created_at as varchar), 0, 14)
			order by count desc`
	} else {
		query = `
			select
				browser,
				count(browser) as count,
				created_at
			from hits
			where
				site=$1 and
				created_at>=$2
			group by browser, strftime('%Y-%m-%d %H', created_at)
			order by count desc`
	}

	var stats []bstat
	err := db.SelectContext(ctx, &stats, query, site.ID, last)
	if err != nil {
		return errors.Wrap(err, "fetch data")
	}

	// Remove everything we'll update; it's faster than running many updates.
	_, err = db.ExecContext(ctx, `delete from browser_stats where site=$1 and day>=$2`,
		site.ID, last)
	if err != nil {
		return errors.Wrap(err, "delete")
	}

	// Group properly.
	type gt struct {
		count   int
		mobile  bool
		day     string
		browser string
		version string
	}
	grouped := map[string]gt{}
	for _, s := range stats {
		browser, version, mobile := getBrowser(s.Browser)
		if browser == "" {
			continue
		}
		k := s.CreatedAt.Format("2006-01-02") + browser + " " + version
		v := grouped[k]
		if v.count == 0 {
			v.day = s.CreatedAt.Format("2006-01-02")
			v.browser = browser
			v.version = version
			v.mobile = mobile
		}
		v.count += s.Count
		grouped[k] = v
	}

	insBrowser := bulk.NewInsert(ctx, zdb.MustGet(ctx).(*sqlx.DB),
		"browser_stats", []string{"site", "day", "browser", "version", "count", "mobile"})
	for _, v := range grouped {
		insBrowser.Values(site.ID, v.day, v.browser, v.version, v.count, v.mobile)
	}

	return insBrowser.Finish()
}

func getBrowser(uaHeader string) (string, string, bool) {
	ua := user_agent.New(uaHeader)
	browser, version := ua.Browser()

	// A lot of this is wrong, so just skip for now.
	if browser == "Android" {
		return "", "", false
	}

	if browser == "Chromium" {
		browser = "Chrome"
	}

	// Correct some wrong data.
	if browser == "Safari" && strings.Count(version, ".") == 3 {
		browser = "Chrome"
	}
	// Note: Safari still shows Chrome and Firefox wrong.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/User-Agent/Firefox
	// https://developer.chrome.com/multidevice/user-agent#chrome_for_ios_user_agent

	// The "build" and "patch" aren't interesting for us, and "minor" hasn't
	// been non-0 since 2010.
	// https://www.chromium.org/developers/version-numbers
	if browser == "Chrome" || browser == "Opera" {
		if i := strings.Index(version, "."); i > -1 {
			version = version[:i]
		}
	}

	// Don't include patch version.
	if browser == "Safari" {
		v := strings.Split(version, ".")
		if len(v) > 2 {
			version = v[0] + "." + v[1]
		}
	}

	mobile := ua.Mobile()
	return browser, version, mobile
}