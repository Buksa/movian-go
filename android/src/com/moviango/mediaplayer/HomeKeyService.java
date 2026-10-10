package com.moviango.mediaplayer;

import android.accessibilityservice.AccessibilityService;
import android.app.PendingIntent;
import android.content.BroadcastReceiver;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.view.KeyEvent;
import android.view.accessibility.AccessibilityEvent;

// HomeKeyService — accessibility service that intercepts the HOME key
// before the system sees it (flagRequestFilterKeyEvents), then brings
// Movian's GLWActivity to the front instead. This is the only HOME
// override that works on operator-locked firmware where the stock
// launcher is pinned by priv-app filter priority and preferred-
// activity records / settings pickers are ignored or stubbed.
// Technique after FTVLaunchX (github.com/codefaktor/FTVLaunchX).
//
// Active only while launcher mode is on (the .HomeLauncher alias is
// enabled). Behaviour:
//   - HOME consumed always → Movian navigates to its home page
//     (page:home); from another app GLWActivity is also brought to
//     front first. Launcher semantics: HOME takes you home.
//   - MENU held + HOME → real stock launcher (escape hatch).
// Wake-from-sleep also re-launches Movian (home app semantics).
public class HomeKeyService extends AccessibilityService {

    private static String foregroundPkg;
    private static boolean menuDown;
    private static long lastLaunch;
    private static BroadcastReceiver screenOnReceiver;

    static boolean aliasEnabled(Context ctx) {
        PackageManager pm = ctx.getPackageManager();
        ComponentName alias = new ComponentName(ctx.getPackageName(),
            ctx.getPackageName() + ".HomeLauncher");
        return pm.getComponentEnabledSetting(alias)
            == PackageManager.COMPONENT_ENABLED_STATE_ENABLED;
    }

    private void launchHome() {
        long now = System.currentTimeMillis();
        if (now - lastLaunch < 200)
            return;
        lastLaunch = now;
        Intent i = new Intent(this, GLWActivity.class);
        i.setFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP |
                   Intent.FLAG_ACTIVITY_EXCLUDE_FROM_RECENTS |
                   Intent.FLAG_ACTIVITY_NEW_TASK |
                   Intent.FLAG_ACTIVITY_REORDER_TO_FRONT |
                   Intent.FLAG_ACTIVITY_RESET_TASK_IF_NEEDED);
        try {
            PendingIntent.getActivity(this, 0, i, PendingIntent.FLAG_IMMUTABLE).send();
        } catch (PendingIntent.CanceledException ignored) {}
    }

    @Override
    public void onAccessibilityEvent(AccessibilityEvent event) {
        if (event.getEventType() == AccessibilityEvent.TYPE_WINDOW_STATE_CHANGED
                && event.getPackageName() != null)
            foregroundPkg = event.getPackageName().toString();
    }

    @Override
    public void onInterrupt() {
    }

    @Override
    protected boolean onKeyEvent(KeyEvent event) {
        if (event.getKeyCode() == KeyEvent.KEYCODE_MENU) {
            menuDown = event.getAction() == KeyEvent.ACTION_DOWN;
            return false;
        }
        if (event.getKeyCode() == KeyEvent.KEYCODE_HOME
                && aliasEnabled(getApplicationContext()) && !menuDown) {
            if (event.getAction() == KeyEvent.ACTION_DOWN) {
                // Always navigate to the Movian home page — launcher
                // semantics: HOME takes you home no matter how deep you
                // are. From another app we also bring Movian to front.
                if (!getPackageName().equals(foregroundPkg))
                    launchHome();
                Core.openUri("page:home");
            }
            return true;
        }
        return false;
    }

    @Override
    protected void onServiceConnected() {
        IntentFilter f = new IntentFilter(Intent.ACTION_SCREEN_ON);
        screenOnReceiver = new BroadcastReceiver() {
            @Override
            public void onReceive(Context c, Intent i) {
                if (aliasEnabled(c))
                    launchHome();
            }
        };
        getApplicationContext().registerReceiver(screenOnReceiver, f);
    }

    @Override
    public boolean onUnbind(Intent intent) {
        if (screenOnReceiver != null) {
            getApplicationContext().unregisterReceiver(screenOnReceiver);
            screenOnReceiver = null;
        }
        return false;
    }
}
