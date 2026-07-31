// Scheduling inbox objects over the sabre schedulingobjects table.

package storage

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"

	"calserver/internal/caldav"
)

// SchedulingObjects reads a principal's schedule inbox from the shared
// schedulingobjects table. Port of PDO getSchedulingObjects.
func (b *CalDAV) SchedulingObjects(principalURI string) ([]caldav.SchedulingObjectInfo, error) {
	defer b.Metrics.DBOpTimer("scheduling_objects")()
	rows, err := b.DB.Query(
		`SELECT uri, calendardata FROM schedulingobjects WHERE principaluri = $1`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []caldav.SchedulingObjectInfo
	for rows.Next() {
		var uri string
		var data []byte
		if err := rows.Scan(&uri, &data); err != nil {
			return nil, err
		}
		out = append(out, caldav.SchedulingObjectInfo{
			URI: uri, PrincipalURI: principalURI, CalendarData: data,
		})
	}
	return out, rows.Err()
}

func (b *CalDAV) SchedulingObject(principalURI, objectURI string) (*caldav.SchedulingObjectInfo, error) {
	var data []byte
	err := b.DB.QueryRow(
		`SELECT calendardata FROM schedulingobjects WHERE principaluri = $1 AND uri = $2`,
		principalURI, objectURI).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &caldav.SchedulingObjectInfo{URI: objectURI, PrincipalURI: principalURI, CalendarData: data}, nil
}

// CreateSchedulingObject transcribes PDO createSchedulingObject: md5 etag,
// size, epoch lastmodified.
func (b *CalDAV) CreateSchedulingObject(principalURI, objectURI string, data []byte) error {
	sum := md5.Sum(data)
	_, err := b.DB.Exec(`
INSERT INTO schedulingobjects (principaluri, calendardata, uri, lastmodified, etag, size)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5)`,
		principalURI, data, objectURI, hex.EncodeToString(sum[:]), len(data))
	return err
}

func (b *CalDAV) DeleteSchedulingObject(principalURI, objectURI string) error {
	_, err := b.DB.Exec(
		`DELETE FROM schedulingobjects WHERE principaluri = $1 AND uri = $2`,
		principalURI, objectURI)
	return err
}
