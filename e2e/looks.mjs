// Which looks a suite runs its checks at.
//
// Nearly every suite runs its whole scenario once per look: a viewport (1280
// or 1440 wide, or a 390px phone) and a colour scheme, four in all. The
// scenario is the functional check (flows, saves, permissions) and the look
// is how the page appears (screenshots, overflow, contrast). Functionality is
// worth testing on every change; the extra looks ran for ten minutes each and
// found layout problems a week later is soon enough to find (WP-313).
//
//   E2E_LOOKS=all   every look (the default, so ./run.sh stays complete)
//   E2E_LOOKS=one   one look per suite: the first of the list, or the one a
//                   suite names with `keep` when that look does the editing.
//                   A suite with a check that only exists on a phone (a menu
//                   that is only there at 390px) keeps a phone look as well:
//                   keep is then a list of positions, [0, 2]
//
// CI runs `one` on pull requests and main, and `all` weekly (ci.yml).
export const everyLook = process.env.E2E_LOOKS !== "one";

// looks(list, keep) is list, or just list[keep] (keep may be a list of
// positions) under E2E_LOOKS=one.
export const looks = (list, keep = 0) =>
  everyLook ? list : [].concat(keep).map((i) => list[i]);
