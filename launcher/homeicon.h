#pragma once

/* Put the Tailscale icon on the home screen if that has not been done yet.
 * Never fails: without an icon everything else still works. */
void home_icon_install_once(void);
