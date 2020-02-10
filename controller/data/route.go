package data

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flynn/flynn/controller/api"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pkg/httphelper"
	hh "github.com/flynn/flynn/pkg/httphelper"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/random"
	router "github.com/flynn/flynn/router/types"
	"github.com/jackc/pgx"
	cjson "github.com/tent/canonical-json-go"
)

var ErrRouteNotFound = errors.New("controller: route not found")

type RouteRepo struct {
	db *postgres.DB
}

func NewRouteRepo(db *postgres.DB) *RouteRepo {
	return &RouteRepo{db: db}
}

// Set takes the desired list of routes for a set of apps, calculates the
// changes that are needed to the existing routes to realise that list, and
// then either atomically applies those changes or returns them for user
// confirmation (otherwise known as a dry run).
//
// The given list of app routes are expected to contain the desired
// configuration for all of the app's routes, and so if any existing routes are
// not contained in the list, or they match ones in the list but have different
// configuration, then they will be either deleted or updated.
//
// If dryRun is true, then the state of all existing routes is calculated and
// returned along with the changes without applying them so that a user can
// inspect the changes and then set the routes again but specifying the state
// that they expect the changes to be applied to, with the request being
// rejected if the state differs.
//
// If dryRun is false, then changes are atomically both calculated and applied,
// first checking that expectedState matches the state of existing routes if
// set.
func (r *RouteRepo) Set(routes []*api.AppRoutes, dryRun bool, expectedState []byte) ([]*api.RouteChange, []byte, error) {
	// check the routes have required fields set
	if err := validateAPIRoutes(routes); err != nil {
		return nil, nil, err
	}

	// if we're doing a dry run, just load the existing routes and return
	// their state along with what changes would be applied
	if dryRun {
		existingRoutes, err := r.List("")
		if err != nil {
			return nil, nil, err
		}
		existingCerts, err := r.ListCertificates()
		if err != nil {
			return nil, nil, err
		}
		state := RouteState(existingRoutes, existingCerts)
		changes, err := r.set(nil, routes, existingRoutes, existingCerts)
		return changes, state, err
	}

	// we're not doing a dry run, so load the existing routes and apply the
	// requested changes using a db transaction
	tx, err := r.db.Begin()
	if err != nil {
		return nil, nil, err
	}

	existingRoutes, err := r.listForUpdate(tx, "")
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	existingCerts, err := r.listCertificatesForUpdate(tx)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}

	// if the request includes an expected state, check it matches the
	// current state of the existing routes
	currentState := RouteState(existingRoutes, existingCerts)
	if len(expectedState) > 0 {
		if !bytes.Equal(expectedState, currentState) {
			tx.Rollback()
			msg := "the expected route state in the request does not match the current state"
			return nil, nil, httphelper.PreconditionFailedErr(msg)
		}
	}

	// set the routes and return the changes
	changes, err := r.set(tx, routes, existingRoutes, existingCerts)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	return changes, currentState, tx.Commit()
}

func (r *RouteRepo) set(tx *postgres.DBTx, desiredAppRoutes []*api.AppRoutes, existingRoutes []*router.Route, existingCerts []*router.Certificate) ([]*api.RouteChange, error) {
	// determine which routes we are going to create, update or delete for each
	// app first so that we can then apply them in the order we want to (e.g.
	// we want to process all deletes before updates and creates to support
	// moving routes between apps)
	var creates []*router.Route
	var updates []*routeUpdate
	var deletes []*router.Route
	for _, appRoutes := range desiredAppRoutes {
		// ensure the app exists
		appID := strings.TrimPrefix(appRoutes.App, "apps/")
		app, err := selectApp(r.db, appID, false)
		if err != nil {
			if err == ErrNotFound {
				err = hh.ValidationErr("", fmt.Sprintf("app not found: %s", appID))
			}
			return nil, err
		}

		// track desired routes that already exist so we know not to create them
		exists := make(map[*api.Route]struct{}, len(appRoutes.Routes))

		// iterate over the app's existing routes to determine what changes to make
		for _, existingRoute := range existingRoutes {
			if existingRoute.ParentRef != ct.RouteParentRefPrefix+app.ID {
				continue
			}

			// we should delete the route unless we find a matching desired route
			shouldDelete := true

			for _, desiredRoute := range appRoutes.Routes {
				// check if the desired route matches the existing route
				if routesMatchForUpdate(existingRoute, desiredRoute) {
					// track that the desired route exists so we don't create it
					exists[desiredRoute] = struct{}{}

					// we shouldn't delete the existing route now that it matches
					shouldDelete = false

					// track this as an update if the configuration differs
					if !routesEqualForUpdate(existingRoute, desiredRoute) {
						update := ToRouterRoute(app.ID, desiredRoute)
						update.ID = existingRoute.ID
						updates = append(updates, &routeUpdate{
							existingRoute: existingRoute,
							updatedRoute:  update,
						})
					}

					break
				}
			}

			// track as a delete if we didn't match with a desired route
			if shouldDelete {
				deletes = append(deletes, existingRoute)
			}
		}

		// track routes to create that don't exist
		for _, route := range appRoutes.Routes {
			if _, ok := exists[route]; ok {
				continue
			}
			creates = append(creates, ToRouterRoute(app.ID, route))
		}
	}

	// process the operations and track the changes made
	var changes []*api.RouteChange

	// process deletions first so they don't affect further validations
	// (e.g. so a domain can be deleted from one app and added to another
	// in the same request)
	for _, routeToDelete := range deletes {
		if err := r.validate(routeToDelete, existingRoutes, existingCerts, routeOpDelete); err != nil {
			return nil, err
		}
		// actually perform the delete if we have a db transaction
		if tx != nil {
			if err := r.deleteTx(tx, routeToDelete); err != nil {
				return nil, err
			}
		}
		changes = append(changes, &api.RouteChange{
			Action: api.RouteChange_ACTION_DELETE,
			Before: api.NewRoute(routeToDelete),
		})
		// remove the deleted route from the existing routes so it no
		// longer affects validations
		newExistingRoutes := make([]*router.Route, 0, len(existingRoutes))
		for _, route := range existingRoutes {
			if route.ID == routeToDelete.ID {
				continue
			}
			newExistingRoutes = append(newExistingRoutes, route)
		}
		existingRoutes = newExistingRoutes
	}

	// process updates
	for _, u := range updates {
		if err := r.validate(u.updatedRoute, existingRoutes, existingCerts, routeOpUpdate); err != nil {
			return nil, err
		}
		// actually perform the update if we have a db transaction
		if tx != nil {
			if err := r.updateTx(tx, u.updatedRoute); err != nil {
				return nil, err
			}
		}
		changes = append(changes, &api.RouteChange{
			Action: api.RouteChange_ACTION_UPDATE,
			Before: api.NewRoute(u.existingRoute),
			After:  api.NewRoute(u.updatedRoute),
		})
		// replace the existing route with the updated one in
		// the existing routes so that it affects future
		// validations
		newExistingRoutes := make([]*router.Route, 0, len(existingRoutes))
		for _, route := range existingRoutes {
			if u.updatedRoute.ID == route.ID {
				newExistingRoutes = append(newExistingRoutes, u.updatedRoute)
			} else {
				newExistingRoutes = append(newExistingRoutes, route)
			}
		}
		existingRoutes = newExistingRoutes
	}

	// process creates
	for _, newRoute := range creates {
		if err := r.validate(newRoute, existingRoutes, existingCerts, routeOpCreate); err != nil {
			return nil, err
		}
		// actually perform the create if we have a db transaction
		if tx != nil {
			if err := r.addTx(tx, newRoute); err != nil {
				return nil, err
			}
		}
		changes = append(changes, &api.RouteChange{
			Action: api.RouteChange_ACTION_CREATE,
			After:  api.NewRoute(newRoute),
		})
		// add the new route to the existing routes so that
		// it affects future validations
		existingRoutes = append(existingRoutes, newRoute)
	}

	return changes, nil
}

func (r *RouteRepo) Add(route *router.Route) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	existingRoutes, err := r.listForUpdate(tx, "")
	if err != nil {
		tx.Rollback()
		return err
	}
	existingCerts, err := r.listCertificatesForUpdate(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := r.validate(route, existingRoutes, existingCerts, routeOpCreate); err != nil {
		tx.Rollback()
		return err
	}
	if err := r.addTx(tx, route); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *RouteRepo) addTx(tx *postgres.DBTx, route *router.Route) error {
	var err error
	switch route.Type {
	case "http":
		err = r.addHTTP(tx, route)
	case "tcp":
		err = r.addTCP(tx, route)
	default:
		return hh.ValidationErr("type", "is invalid (must be 'http' or 'tcp')")
	}
	if err != nil {
		return err
	}
	return r.createEvent(tx, route, ct.EventTypeRoute)
}

func (r *RouteRepo) addHTTP(tx *postgres.DBTx, route *router.Route) error {
	if err := tx.QueryRow(
		"http_route_insert",
		route.ParentRef,
		route.Service,
		route.Port,
		route.Leader,
		route.DrainBackends,
		route.Domain,
		route.Sticky,
		route.Path,
		route.DisableKeepAlives,
	).Scan(&route.ID, &route.Path, &route.CreatedAt, &route.UpdatedAt); err != nil {
		return err
	}
	return r.addRouteCertWithTx(tx, route)
}

func (r *RouteRepo) addTCP(tx *postgres.DBTx, route *router.Route) error {
	return tx.QueryRow(
		"tcp_route_insert",
		route.ParentRef,
		route.Service,
		route.Port,
		route.Leader,
		route.DrainBackends,
	).Scan(&route.ID, &route.Port, &route.CreatedAt, &route.UpdatedAt)
}

var nonAlphaNumPattern = regexp.MustCompile(`[^a-zA-Z\d]+`)

// genCertRef generates a ref for the given certificate as:
//
//   <cn>_v<n>
//
// where <cn> is the Subject Common Name from the certificate (sanitized to
// include only letters, digits and underscores) and <n> is one more than the
// number of existing certificates with <cn> as the prefix.
//
// For example:
//
//   app_example_com_v1
//
// If the Subject Common Name isn't set or can't be determined, then the
// current date is used instead.
//
// If the number of existing certificates with <cn> as the prefix can't be
// determined, then a random string is used instead.
func genCertRef(db dbOrTx, cert, key string) string {
	// determine the Subject Common Name
	cn := func() string {
		c, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return ""
		}
		if len(c.Certificate) == 0 {
			return ""
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			return ""
		}
		return leaf.Subject.CommonName
	}()

	// fallback to the current date
	if cn == "" {
		cn = time.Now().Format("20060102")
	}

	// sanitise the common name
	cn = nonAlphaNumPattern.ReplaceAllString(cn, "_")

	// determine the version, falling back to a random string
	var n int64
	if err := db.QueryRow("certificate_count_by_ref_prefix", cn).Scan(&n); err != nil {
		return cn + "_v" + random.String(8)
	}

	return cn + "_v" + strconv.FormatInt(n+1, 10)
}

func (r *RouteRepo) addCert(db dbOrTx, cert *router.Certificate) error {
	tlsCertSHA256 := sha256.Sum256([]byte(cert.Cert))
	if cert.Ref == "" {
		cert.Ref = genCertRef(db, cert.Cert, cert.Key)
	}
	if cert.Meta == nil {
		cert.Meta = make(map[string]string)
	}
	if err := db.QueryRow(
		"certificate_insert",
		cert.Cert,
		cert.Key,
		tlsCertSHA256[:],
		cert.Ref,
		cert.Meta,
	).Scan(&cert.ID, &cert.CreatedAt, &cert.UpdatedAt); err != nil {
		return err
	}
	for _, rid := range cert.Routes {
		if err := db.Exec("route_certificate_delete_by_route_id", rid); err != nil {
			return err
		}
		if err := db.Exec("route_certificate_insert", rid, cert.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *RouteRepo) addRouteCertWithTx(tx *postgres.DBTx, route *router.Route) error {
	// if the route has a certificate ref, load it from the database
	if ref := route.CertificateRef; ref != "" {
		cert, err := scanCertificate(tx.QueryRow("certificate_select", ref))
		if err != nil {
			return err
		}
		route.Certificate = cert
	}

	// create the certificate if set
	cert := route.Certificate
	if cert == nil || (len(cert.Cert) == 0 && len(cert.Key) == 0) {
		return nil
	}
	cert.Routes = []string{route.ID}
	if err := r.addCert(tx, cert); err != nil {
		return err
	}
	return nil
}

func (r *RouteRepo) Get(typ, id string) (*router.Route, error) {
	if id == "" {
		return nil, ErrRouteNotFound
	}
	var (
		route *router.Route
		err   error
	)
	switch typ {
	case "http":
		route, err = r.getHTTP(id)
	case "tcp":
		route, err = r.getTCP(id)
	default:
		err = ErrRouteNotFound
	}
	if err == pgx.ErrNoRows {
		err = ErrRouteNotFound
	}
	return route, err
}

func (r *RouteRepo) getHTTP(id string) (*router.Route, error) {
	return scanHTTPRoute(r.db.QueryRow("http_route_select", id))
}

func scanHTTPRoute(s postgres.Scanner) (*router.Route, error) {
	var (
		route         router.Route
		certID        *string
		certRoutes    *string
		certCert      *string
		certKey       *string
		certRef       *string
		certMeta      *map[string]string
		certCreatedAt *time.Time
		certUpdatedAt *time.Time
	)
	if err := s.Scan(
		&route.ID,
		&route.ParentRef,
		&route.Service,
		&route.Port,
		&route.Leader,
		&route.DrainBackends,
		&route.Domain,
		&route.Sticky,
		&route.Path,
		&route.DisableKeepAlives,
		&route.CreatedAt,
		&route.UpdatedAt,
		&certID,
		&certRoutes,
		&certCert,
		&certKey,
		&certRef,
		&certMeta,
		&certCreatedAt,
		&certUpdatedAt,
	); err != nil {
		return nil, err
	}
	route.Type = "http"
	if certID != nil {
		route.CertificateRef = *certRef
		route.Certificate = &router.Certificate{
			ID:        *certID,
			Routes:    splitPGStringArray(*certRoutes),
			Cert:      *certCert,
			Key:       *certKey,
			Ref:       *certRef,
			Meta:      *certMeta,
			CreatedAt: *certCreatedAt,
			UpdatedAt: *certUpdatedAt,
		}
	}
	return &route, nil
}

func (r *RouteRepo) getTCP(id string) (*router.Route, error) {
	return scanTCPRoute(r.db.QueryRow("tcp_route_select", id))
}

func scanTCPRoute(s postgres.Scanner) (*router.Route, error) {
	var route router.Route
	if err := s.Scan(
		&route.ID,
		&route.ParentRef,
		&route.Service,
		&route.Port,
		&route.Leader,
		&route.DrainBackends,
		&route.CreatedAt,
		&route.UpdatedAt,
	); err != nil {
		return nil, err
	}
	route.Type = "tcp"
	return &route, nil
}

func (r *RouteRepo) List(parentRef string) ([]*router.Route, error) {
	return r.list(r.db, parentRef, false)
}

func (r *RouteRepo) listForUpdate(db rowQueryer, parentRef string) ([]*router.Route, error) {
	return r.list(db, parentRef, true)
}

func (r *RouteRepo) list(db rowQueryer, parentRef string, forUpdate bool) ([]*router.Route, error) {
	httpRoutes, err := r.listHTTP(db, parentRef, forUpdate)
	if err != nil {
		return nil, err
	}
	tcpRoutes, err := r.listTCP(db, parentRef, forUpdate)
	if err != nil {
		return nil, err
	}
	return append(httpRoutes, tcpRoutes...), nil
}

func (r *RouteRepo) listHTTP(db rowQueryer, parentRef string, forUpdate bool) ([]*router.Route, error) {
	query := "http_route_list"
	var args []interface{}
	if forUpdate {
		query += "_for_update"
	}
	if parentRef != "" {
		query += "_by_parent_ref"
		args = append(args, parentRef)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []*router.Route
	for rows.Next() {
		route, err := scanHTTPRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (r *RouteRepo) listTCP(db rowQueryer, parentRef string, forUpdate bool) ([]*router.Route, error) {
	query := "tcp_route_list"
	var args []interface{}
	if forUpdate {
		query += "_for_update"
	}
	if parentRef != "" {
		query += "_by_parent_ref"
		args = append(args, parentRef)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []*router.Route
	for rows.Next() {
		route, err := scanTCPRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (r *RouteRepo) Update(route *router.Route) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	existingRoutes, err := r.listForUpdate(tx, "")
	if err != nil {
		tx.Rollback()
		return err
	}
	existingCerts, err := r.listCertificatesForUpdate(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := r.validate(route, existingRoutes, existingCerts, routeOpUpdate); err != nil {
		tx.Rollback()
		return err
	}
	if err := r.updateTx(tx, route); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *RouteRepo) updateTx(tx *postgres.DBTx, route *router.Route) error {
	var err error
	switch route.Type {
	case "http":
		err = r.updateHTTP(tx, route)
	case "tcp":
		err = r.updateTCP(tx, route)
	default:
		err = ErrRouteNotFound
	}
	if err == pgx.ErrNoRows {
		err = ErrRouteNotFound
	}
	if err != nil {
		return err
	}
	return r.createEvent(tx, route, ct.EventTypeRoute)
}

func (r *RouteRepo) updateHTTP(tx *postgres.DBTx, route *router.Route) error {
	if err := tx.QueryRow(
		"http_route_update",
		route.ParentRef,
		route.Service,
		route.Port,
		route.Leader,
		route.Sticky,
		route.Path,
		route.DrainBackends,
		route.DisableKeepAlives,
		route.ID,
		route.Domain,
	).Scan(
		&route.ID,
		&route.ParentRef,
		&route.Service,
		&route.Port,
		&route.Leader,
		&route.DrainBackends,
		&route.Domain,
		&route.Sticky,
		&route.Path,
		&route.DisableKeepAlives,
		&route.CreatedAt,
		&route.UpdatedAt,
	); err != nil {
		return err
	}
	return r.addRouteCertWithTx(tx, route)
}

func (r *RouteRepo) updateTCP(tx *postgres.DBTx, route *router.Route) error {
	return tx.QueryRow(
		"tcp_route_update",
		route.ParentRef,
		route.Service,
		route.Port,
		route.Leader,
		route.DrainBackends,
		route.ID,
	).Scan(
		&route.ID,
		&route.ParentRef,
		&route.Service,
		&route.Port,
		&route.Leader,
		&route.DrainBackends,
		&route.CreatedAt,
		&route.UpdatedAt,
	)
}

func (r *RouteRepo) Delete(route *router.Route) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	existingRoutes, err := r.listForUpdate(tx, "")
	if err != nil {
		tx.Rollback()
		return err
	}
	existingCerts, err := r.listCertificatesForUpdate(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := r.validate(route, existingRoutes, existingCerts, routeOpDelete); err != nil {
		tx.Rollback()
		return err
	}
	if err := r.deleteTx(tx, route); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *RouteRepo) deleteTx(tx *postgres.DBTx, route *router.Route) error {
	var err error
	switch route.Type {
	case "http":
		err = tx.Exec("http_route_delete", route.ID)
	case "tcp":
		err = tx.Exec("tcp_route_delete", route.ID)
	default:
		err = ErrRouteNotFound
	}
	if err != nil {
		return err
	}
	return r.createEvent(tx, route, ct.EventTypeRouteDeletion)
}

func (r *RouteRepo) ListCertificates() ([]*router.Certificate, error) {
	return r.listCertificates(r.db, false)
}

func (r *RouteRepo) listCertificatesForUpdate(db dbOrTx) ([]*router.Certificate, error) {
	return r.listCertificates(db, true)
}

func (r *RouteRepo) listCertificates(db dbOrTx, forUpdate bool) ([]*router.Certificate, error) {
	query := "certificate_list"
	if forUpdate {
		query += "_for_update"
	}
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var certs []*router.Certificate
	for rows.Next() {
		cert, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return certs, nil
}

func scanCertificate(s postgres.Scanner) (*router.Certificate, error) {
	var (
		cert   router.Certificate
		routes string
	)
	if err := s.Scan(
		&cert.ID,
		&routes,
		&cert.Cert,
		&cert.Key,
		&cert.Ref,
		&cert.Meta,
		&cert.CreatedAt,
		&cert.UpdatedAt,
	); err != nil {
		return nil, err
	}
	cert.Routes = splitPGStringArray(routes)
	if cert.Meta == nil {
		// ensure `{}` rather than `null` when serializing to JSON
		cert.Meta = map[string]string{}
	}
	return &cert, nil
}

func (r *RouteRepo) GetCertificate(name string) (*router.Certificate, error) {
	return r.getCert(r.db, name, false)
}

func (r *RouteRepo) getCert(db dbOrTx, name string, forUpdate bool) (*router.Certificate, error) {
	query := "certificate_select"
	if forUpdate {
		query += "_for_update"
	}
	id := strings.TrimPrefix(name, "certificates/")
	cert, err := scanCertificate(db.QueryRow(query, id))
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return cert, err
}

func (r *RouteRepo) AddCertificate(cert *router.Certificate, force bool) error {
	if err := r.validateCert(cert, force); err != nil {
		return err
	}
	return r.addCert(r.db, cert)
}

func (r *RouteRepo) DeleteCertificate(name string) (*router.Certificate, error) {
	// start a transaction
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}

	// get the certificate
	cert, err := r.getCert(tx, name, true)
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	// ensure the certificate is not referenced by any routes
	if len(cert.Routes) > 0 {
		tx.Rollback()
		return nil, fmt.Errorf("cannot delete certificate as it is referenced by the following routes: %s", strings.Join(cert.Routes, ", "))
	}

	// delete the certificate
	if err := tx.Exec("certificate_delete", cert.ID); err != nil {
		tx.Rollback()
		return nil, err
	}

	// commit the transaction
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return cert, nil
}

func (r *RouteRepo) createEvent(tx *postgres.DBTx, route *router.Route, typ ct.EventType) error {
	var appID string
	if strings.HasPrefix(route.ParentRef, ct.RouteParentRefPrefix) {
		appID = strings.TrimPrefix(route.ParentRef, ct.RouteParentRefPrefix)
	}
	hash := md5.New()
	io.WriteString(hash, appID)
	io.WriteString(hash, string(typ))
	io.WriteString(hash, route.ID)
	io.WriteString(hash, route.CreatedAt.String())
	io.WriteString(hash, route.UpdatedAt.String())
	uniqueID := fmt.Sprintf("%x", hash.Sum(nil))
	return CreateEvent(tx.Exec, &ct.Event{
		AppID:      appID,
		ObjectID:   route.ID,
		ObjectType: typ,
		UniqueID:   uniqueID,
	}, route)
}

// validateAPIRoutes checks that the given API routes are semantically valid
func validateAPIRoutes(appRoutes []*api.AppRoutes) error {
	for _, a := range appRoutes {
		if a.App == "" {
			return hh.ValidationErr("app", "must be set")
		}
		for _, route := range a.Routes {
			if route.ServiceTarget == nil {
				return hh.ValidationErr("service_target", "must be set")
			}
			switch config := route.Config.(type) {
			case *api.Route_Http:
				if config.Http == nil {
					return hh.ValidationErr("config.http", "must be set for HTTP routes")
				}
				if config.Http.Domain == "" {
					return hh.ValidationErr("config.http.domain", "must be set for HTTP routes")
				}
				// ensure HTTP routes have a normalised path
				config.Http.Path = normaliseRoutePath(config.Http.Path)
			case *api.Route_Tcp:
				if config.Tcp == nil {
					return hh.ValidationErr("config.tcp", "must be set for TCP routes")
				}
				if config.Tcp.Port == nil {
					return hh.ValidationErr("config.tcp.port", "must be set for TCP routes")
				}
			default:
				return hh.ValidationErr("config", "must be either HTTP or TCP")
			}
		}
	}
	return nil
}

// routeOp represents an operation that is performed on a route and is used to
// decide what type of validation to perform
type routeOp string

const (
	routeOpCreate routeOp = "create"
	routeOpUpdate routeOp = "update"
	routeOpDelete routeOp = "delete"
)

// validate validates the given route against the list of existing routes and
// certificates for the given operation
func (r *RouteRepo) validate(route *router.Route, existingRoutes []*router.Route, existingCerts []*router.Certificate, op routeOp) error {
	switch route.Type {
	case "http":
		return r.validateHTTP(route, existingRoutes, existingCerts, op)
	case "tcp":
		return r.validateTCP(route, existingRoutes, op)
	default:
		return hh.ValidationErr("type", "is invalid (must be 'http' or 'tcp')")
	}
}

// validateHTTP validates an HTTP route
func (r *RouteRepo) validateHTTP(route *router.Route, existingRoutes []*router.Route, existingCerts []*router.Certificate, op routeOp) error {
	if op == routeOpDelete {
		// If we are removing a default route ensure no dependent routes left
		if route.Path == "/" {
			for _, existing := range existingRoutes {
				if existing.Domain == route.Domain && existing.Path != "/" {
					return hh.ValidationErr("", fmt.Sprintf(
						"cannot delete default route as a dependent route with path=%s exists",
						existing.Path,
					))
				}
			}
		}
		// don't do any further validation on a route we're deleting
		return nil
	}

	// check the domain and service are set
	if route.Domain == "" {
		return hh.ValidationErr("domain", "must be set")
	}
	if route.Service == "" {
		return hh.ValidationErr("service", "must be set")
	}

	// check the default port is used
	if route.Port > 0 {
		return hh.ValidationErr("port", "must have the default value of zero")
	}

	// normalise the path
	route.Path = normaliseRoutePath(route.Path)

	// path must start with a slash
	if route.Path[0] != '/' {
		return hh.ValidationErr("path", "must start with a forward slash")
	}

	// check routes are unique on domain, port and path
	for _, existing := range existingRoutes {
		if existing.Type != "http" || (op == routeOpUpdate && existing.ID == route.ID) {
			continue
		}
		if existing.Domain == route.Domain && existing.Port == route.Port && existing.Path == route.Path {
			return hh.ConflictErr(fmt.Sprintf("a http route with domain=%s and path=%s already exists", route.Domain, route.Path))
		}
	}

	// If path not the default then validate that a default route exists
	if route.Path != "/" {
		defaultExists := false
		for _, existing := range existingRoutes {
			if existing.Type == "http" && existing.Domain == route.Domain && existing.Path == "/" {
				defaultExists = true
				break
			}
		}
		if !defaultExists {
			return hh.ValidationErr("path", "is not allowed as there is no route at the default path")
		}
	}

	// check that all routes with the same service have the same drain_backends
	for _, existing := range existingRoutes {
		if existing.Type == "http" && existing.Service == route.Service && existing.DrainBackends != route.DrainBackends {
			msg := fmt.Sprintf(
				"cannot create route with mismatch drain_backends=%v, other routes for service %s exist with drain_backends=%v",
				route.DrainBackends, route.Service, existing.DrainBackends,
			)
			return hh.ValidationErr("drain_backends", msg)
		}
	}

	// handle legacy certificate fields
	if route.LegacyTLSCert != "" || route.LegacyTLSKey != "" {
		// setting both legacy and route.Certificate is an error
		if route.Certificate != nil {
			return hh.ValidationErr("certificate", "cannot be set along with the deprecated tls_cert and tls_key")
		}
		route.Certificate = &router.Certificate{
			Cert: route.LegacyTLSCert,
			Key:  route.LegacyTLSKey,
		}
	}

	// validate the certificate if set
	if ref := route.CertificateRef; ref != "" {
		exists := false
		for _, c := range existingCerts {
			if c.Ref == ref {
				exists = true
				break
			}
		}
		if !exists {
			return hh.ValidationErr("certificate", fmt.Sprintf("not found: %s", ref))
		}
	}
	cert := route.Certificate
	if cert != nil && len(cert.Cert) > 0 && len(cert.Key) > 0 {
		if err := r.validateCert(cert, false); err != nil {
			return err
		}
	}

	return nil
}

// validateTCP validates a TCP route
func (r *RouteRepo) validateTCP(route *router.Route, existingRoutes []*router.Route, op routeOp) error {
	// don't validate routes that are being deleted
	if op == routeOpDelete {
		return nil
	}

	// don't allow default HTTP ports
	if route.Port == 80 || route.Port == 443 {
		return hh.ConflictErr("Port reserved for HTTP/HTTPS traffic")
	}

	// assign an available port if the port is unset
	if route.Port == 0 {
	outer:
		for port := int32(3000); port <= 3500; port++ {
			for _, existing := range existingRoutes {
				if existing.Type == "tcp" && existing.Port == port {
					continue outer
				}
			}
			route.Port = port
			break
		}
	}

	// check that the port is in range
	if route.Port <= 0 || route.Port >= 65535 {
		return hh.ValidationErr("port", "must be between 0 and 65535")
	}

	// check that the port is unused
	for _, existing := range existingRoutes {
		if existing.Type != "tcp" || (op == routeOpUpdate && existing.ID == route.ID) {
			continue
		}
		if existing.Port == route.Port {
			return hh.ConflictErr(fmt.Sprintf("a tcp route with port=%d already exists", route.Port))
		}
	}

	// check the service is set
	if route.Service == "" {
		return hh.ValidationErr("service", "must be set")
	}

	// check that all routes with the same service have the same drain_backends
	for _, existing := range existingRoutes {
		if existing.Type == "tcp" && existing.Service == route.Service && existing.DrainBackends != route.DrainBackends {
			msg := fmt.Sprintf(
				"cannot create route with mismatch drain_backends=%v, other routes for service %s exist with drain_backends=%v",
				route.DrainBackends, route.Service, existing.DrainBackends,
			)
			return hh.ValidationErr("drain_backends", msg)
		}
	}

	return nil
}

func (r *RouteRepo) validateCert(cert *router.Certificate, force bool) error {
	// trim any whitespace from the cert and key
	cert.Cert = strings.Trim(cert.Cert, " \n")
	cert.Key = strings.Trim(cert.Key, " \n")

	// expect a static certificate
	v, ok := api.NewCertificate(cert).Certificate.(*api.Certificate_Static)
	if !ok {
		return hh.ValidationErr("certificate", "must be a static certificate")
	}
	staticCert := v.Static

	// reject certificates with invalid status
	if staticCert.Status == api.StaticCertificate_STATUS_INVALID {
		return hh.ValidationErr("certificate", fmt.Sprintf("is invalid: %s", staticCert.StatusDetail))
	}

	return nil
}

// normaliseRoutePath normalises a route path by ensuring it ends with a
// forward slash
func normaliseRoutePath(path string) string {
	if !strings.HasSuffix(path, "/") {
		return path + "/"
	}
	return path
}

// routeStateData is used to calculate the state of a set of routes and
// certificates
type routeStateData struct {
	Routes       map[string]*router.Route       `json:"routes,omitempty"`
	Certificates map[string]*router.Certificate `json:"certificates,omitempty"`
}

// RouteState calculates the state of the given set of routes and certificates
// as the SHA256 digest of the canonical JSON representation of a map of route
// IDs to routes and certificate IDs to certificates
func RouteState(routes []*router.Route, certs []*router.Certificate) []byte {
	v := routeStateData{
		Routes:       make(map[string]*router.Route, len(routes)),
		Certificates: make(map[string]*router.Certificate, len(certs)),
	}
	for _, r := range routes {
		v.Routes[r.ID] = r
	}
	for _, c := range certs {
		v.Certificates[c.ID] = c
	}
	data, _ := cjson.Marshal(v)
	state := sha256.Sum256(data)
	return state[:]
}

// routeUpdate is used to track existing routes that need to be updated along
// with their updated route
type routeUpdate struct {
	existingRoute *router.Route
	updatedRoute  *router.Route
}

// routesMatchForUpdate checks whether an existing route matches the given
// desired route and should thus be updated with it
func routesMatchForUpdate(existing *router.Route, desired *api.Route) bool {
	switch config := desired.Config.(type) {
	case *api.Route_Http:
		// HTTP routes should be updated with the desired route if they
		// have the same domain and path
		return config.Http.Domain == existing.Domain && config.Http.Path == existing.Path
	case *api.Route_Tcp:
		// TCP routes should be updated with the desired route if they
		// have the same port
		return int32(config.Tcp.Port.Port) == existing.Port
	default:
		return false
	}
}

// routesEqualForUpdate checks whether an existing route has the same
// configuration as a desired route that has been identified as being an update
// of the existing route
func routesEqualForUpdate(existing *router.Route, desired *api.Route) bool {
	// check HTTP routes for a change in certificate or stickiness
	if config, ok := desired.Config.(*api.Route_Http); ok {
		if config.Http.Tls == nil && existing.CertificateRef != "" {
			return false
		}
		if tls := config.Http.Tls; tls != nil && strings.TrimPrefix(tls.Certificate, "certificates/") != existing.CertificateRef {
			return false
		}
		if existing.Sticky == (config.Http.StickySessions == nil) {
			return false
		}
	}

	// check general config is the same
	return existing.Service == desired.ServiceTarget.ServiceName &&
		existing.Leader == desired.ServiceTarget.Leader &&
		existing.DrainBackends == desired.ServiceTarget.DrainBackends &&
		existing.DisableKeepAlives == desired.DisableKeepAlives
}

// ToRouterRoute converts an api.Route into a router.Route
func ToRouterRoute(appID string, route *api.Route) *router.Route {
	r := &router.Route{
		ParentRef:         ct.RouteParentRefPrefix + appID,
		DisableKeepAlives: route.DisableKeepAlives,
	}
	if t := route.ServiceTarget; t != nil {
		r.Service = t.ServiceName
		r.Leader = t.Leader
		r.DrainBackends = t.DrainBackends
	}
	switch config := route.Config.(type) {
	case *api.Route_Http:
		r.Type = "http"
		r.Domain = config.Http.Domain
		r.Path = config.Http.Path
		r.Sticky = config.Http.StickySessions != nil
		if tls := config.Http.Tls; tls != nil && tls.Certificate != "" {
			r.CertificateRef = strings.TrimPrefix(tls.Certificate, "certificates/")
		}
	case *api.Route_Tcp:
		r.Type = "tcp"
		r.Port = int32(config.Tcp.Port.Port)
	}
	return r
}
