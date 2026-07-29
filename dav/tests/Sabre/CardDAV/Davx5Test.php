<?php

declare(strict_types=1);

namespace Sabre\CardDAV;

use Sabre\HTTP;

/**
 * DAVx5 (Android) CardDAV request shapes, recorded against sabre so the Go
 * port can be replayed for parity. These mirror the real request sequence
 * DAVx5 issues when syncing an address book: enumerate books, read collection
 * properties, sync-collection for etags, then addressbook-multiget for card
 * data (in both the default vCard 3.0 and the negotiated 4.0 forms).
 *
 * The fixture includes a group/list card (X-ADDRESSBOOKSERVER-KIND:group) so
 * group sync is exercised end to end.
 */
class Davx5Test extends \Sabre\AbstractDAVServerTestCase
{
    protected $setupCardDAV = true;
    protected $setupACL = true;
    protected $autoLogin = 'user1';

    protected $carddavAddressBooks = [
        [
            'id' => 'book1',
            'uri' => 'book1',
            'principaluri' => 'principals/user1',
            '{DAV:}displayname' => 'My Contacts',
        ],
    ];
    protected $carddavCards = [
        'book1' => [
            'card1' => "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:11111111\r\nFN:Alice Example\r\nN:Example;Alice;;;\r\nEMAIL;TYPE=WORK:alice@example.com\r\nTEL;TYPE=CELL:+15550001\r\nEND:VCARD\r\n",
            'card2' => "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:22222222\r\nFN:Bob Example\r\nN:Example;Bob;;;\r\nEMAIL;TYPE=HOME:bob@example.com\r\nEND:VCARD\r\n",
            'group1' => "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:99999999\r\nFN:Work\r\nN:Work\r\nX-ADDRESSBOOKSERVER-KIND:group\r\nX-ADDRESSBOOKSERVER-MEMBER:urn:uuid:11111111\r\nX-ADDRESSBOOKSERVER-MEMBER:urn:uuid:22222222\r\nEND:VCARD\r\n",
        ],
    ];

    // DAVx5 enumerates address books with a Depth:1 PROPFIND on the home set.
    public function testDavx5PropfindHome()
    {
        $request = new HTTP\Request('PROPFIND', '/addressbooks/user1/', ['Depth' => '1']);
        $request->setBody(
            '<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cs="http://calendarserver.org/ns/">
  <d:prop>
    <d:resourcetype/>
    <d:displayname/>
    <card:addressbook-description/>
    <cs:getctag/>
    <d:sync-token/>
    <d:current-user-privilege-set/>
    <d:supported-report-set/>
    <card:max-resource-size/>
    <card:supported-address-data/>
    <card:supported-collation-set/>
  </d:prop>
</d:propfind>'
        );
        $this->request($request, 207);
    }

    // Depth:0 PROPFIND on the address book itself for the sync/ctag properties.
    public function testDavx5PropfindAddressbook()
    {
        $request = new HTTP\Request('PROPFIND', '/addressbooks/user1/book1', ['Depth' => '0']);
        $request->setBody(
            '<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/">
  <d:prop>
    <cs:getctag/>
    <d:sync-token/>
  </d:prop>
</d:propfind>'
        );
        $this->request($request, 207);
    }

    // Note: DAVx5's initial sync uses a sync-collection REPORT; its shape is
    // already covered by the green Sabre\DAV\Sync\PluginTest corpus cases (the
    // CardDAV Mock backend here is not SyncSupport, so it cannot be recorded
    // against an address book). PROPFIND-based enumeration + multiget below is
    // DAVx5's fallback path and the CardDAV-specific surface.

    // addressbook-multiget for card data in the default vCard 3.0 form.
    public function testDavx5Multiget()
    {
        $request = new HTTP\Request('REPORT', '/addressbooks/user1/book1', ['Depth' => '1']);
        $request->setBody(
            '<?xml version="1.0" encoding="UTF-8"?>
<card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:getetag/>
    <card:address-data/>
  </d:prop>
  <d:href>/addressbooks/user1/book1/card1</d:href>
  <d:href>/addressbooks/user1/book1/card2</d:href>
  <d:href>/addressbooks/user1/book1/group1</d:href>
</card:addressbook-multiget>'
        );
        $this->request($request, 207);
    }

    // addressbook-multiget negotiating vCard 4.0 (DAVx5's vCard4 mode).
    public function testDavx5MultigetVcard4()
    {
        $request = new HTTP\Request('REPORT', '/addressbooks/user1/book1', ['Depth' => '1']);
        $request->setBody(
            '<?xml version="1.0" encoding="UTF-8"?>
<card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:getetag/>
    <card:address-data content-type="text/vcard" version="4.0"/>
  </d:prop>
  <d:href>/addressbooks/user1/book1/card1</d:href>
  <d:href>/addressbooks/user1/book1/card2</d:href>
</card:addressbook-multiget>'
        );
        $this->request($request, 207);
    }
}
