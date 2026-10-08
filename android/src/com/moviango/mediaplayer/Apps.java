package com.moviango.mediaplayer;

import java.io.File;
import java.io.FileOutputStream;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.os.Build;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.drawable.BitmapDrawable;
import android.graphics.drawable.Drawable;
import android.provider.Settings;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONObject;

// Launcher support: enumerate launchable apps (LAUNCHER + LEANBACK_LAUNCHER),
// export icons as PNG into the cache dir, launch packages, open the system
// home-settings screen. All entry points are static JNI targets for the Go
// core (internal/arch/android_jni.go).
public class Apps {

    private static Context ctx;
    private static File iconDir;

    public static void init(Context c) {
        ctx = c.getApplicationContext();
        iconDir = new File(c.getCacheDir(), "appicons");
        iconDir.mkdirs();
    }

    // list() — JSON array of {pkg,label,icon}; icon is a file:// URL to a
    // cached PNG (written lazily on first listing).
    public static String list() {
        JSONArray arr = new JSONArray();
        if (ctx == null)
            return arr.toString();
        PackageManager pm = ctx.getPackageManager();
        Set<String> seen = new HashSet<>();
        String[] cats = {Intent.CATEGORY_LAUNCHER,
                         Intent.CATEGORY_LEANBACK_LAUNCHER};
        for (String cat : cats) {
            Intent probe = new Intent(Intent.ACTION_MAIN);
            probe.addCategory(cat);
            List<ResolveInfo> ris;
            try {
                ris = pm.queryIntentActivities(probe, 0);
            } catch (Exception e) {
                continue;
            }
            for (ResolveInfo ri : ris) {
                String pkg = ri.activityInfo.packageName;
                if (pkg == null || pkg.equals(ctx.getPackageName()) ||
                    !seen.add(pkg))
                    continue;
                try {
                    JSONObject o = new JSONObject();
                    o.put("pkg", pkg);
                    CharSequence label = ri.loadLabel(pm);
                    o.put("label", label != null ? label.toString() : pkg);
                    File f = new File(iconDir, pkg + ".png");
                    if (!f.exists())
                        writeIcon(pm, pkg, f);
                    if (f.exists())
                        o.put("icon", "file://" + f.getAbsolutePath());
                    arr.put(o);
                } catch (Exception ignored) {}
            }
        }
        Log.d("Movian", "Apps.list: " + arr.length() + " apps");
        return arr.toString();
    }

    private static void writeIcon(PackageManager pm, String pkg, File f) {
        try {
            Drawable d = pm.getApplicationIcon(pkg);
            Bitmap b;
            if (d instanceof BitmapDrawable && ((BitmapDrawable) d).getBitmap() != null) {
                b = ((BitmapDrawable) d).getBitmap();
            } else {
                int w = Math.max(1, d.getIntrinsicWidth());
                int h = Math.max(1, d.getIntrinsicHeight());
                b = Bitmap.createBitmap(w, h, Bitmap.Config.ARGB_8888);
                Canvas c = new Canvas(b);
                d.setBounds(0, 0, w, h);
                d.draw(c);
            }
            FileOutputStream out = new FileOutputStream(f);
            b.compress(Bitmap.CompressFormat.PNG, 100, out);
            out.close();
        } catch (Exception ignored) {}
    }

    // launch(pkg) — start the app's launcher activity.
    public static boolean launch(String pkg) {
        if (ctx == null)
            return false;
        Intent i = ctx.getPackageManager().getLaunchIntentForPackage(pkg);
        if (i == null)
            return false;
        i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        try {
            ctx.startActivity(i);
            return true;
        } catch (Exception e) {
            return false;
        }
    }

    // openSettings() — bring up the system Settings app.
    public static void openSettings() {
        if (ctx == null)
            return;
        try {
            Intent i = new Intent(Settings.ACTION_SETTINGS);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            ctx.startActivity(i);
        } catch (Exception e) {
            Log.w("Apps", "openSettings failed", e);
        }
    }

    // homeAlias — the disabled-by-default activity-alias carrying the
    // HOME/DEFAULT filter (see AndroidManifest). Movian appears in the
    // launcher pickers only while this component is enabled.
    private static ComponentName homeAlias() {
        return new ComponentName(ctx.getPackageName(),
                                 ctx.getPackageName() + ".HomeLauncher");
    }

    // setLauncherAlias(on) — enable/disable the HOME alias. Pure
    // component-state change: no intent is fired, the running activity
    // is untouched.
    public static void setLauncherAlias(int on) {
        if (ctx == null)
            return;
        try {
            ctx.getPackageManager().setComponentEnabledSetting(
                homeAlias(),
                on != 0 ? PackageManager.COMPONENT_ENABLED_STATE_ENABLED
                        : PackageManager.COMPONENT_ENABLED_STATE_DISABLED,
                PackageManager.DONT_KILL_APP);
        } catch (Exception e) {
            Log.w("Apps", "setLauncherAlias failed", e);
        }
    }

    // openHomeSettings() — bring up the "select home app" UI.
    //
    // Fallback chain, from most to least standard:
    //   1. RoleManager HOME role request (API 29+) — official dialog.
    //   2. Settings.ACTION_HOME_SETTINGS — the stock "Home app" picker.
    //      Skipped when it resolves to a do-nothing OEM stub (the TIM
    //      box points it at an EmptyStubActivity).
    //   3. createChooser over a HOME intent — forces a picker even when
    //      a default is recorded.
    //   4. Bare HOME intent — last resort, resolves to the current
    //      default on locked firmware.
    // Note: on operator boxes (e.g. TIM) the stock launcher is a
    // priv-app whose HOME filter priority always wins and preferred-
    // activity records are ignored — no app can override it there.
    public static void openHomeSettings() {
        if (ctx == null)
            return;
        // Make sure Movian is a candidate, then cycle the alias so the
        // resolve set changes and any recorded default is invalidated.
        try {
            PackageManager pm = ctx.getPackageManager();
            pm.setComponentEnabledSetting(
                homeAlias(),
                PackageManager.COMPONENT_ENABLED_STATE_ENABLED,
                PackageManager.DONT_KILL_APP);
            pm.setComponentEnabledSetting(
                homeAlias(),
                PackageManager.COMPONENT_ENABLED_STATE_DISABLED,
                PackageManager.DONT_KILL_APP);
            pm.setComponentEnabledSetting(
                homeAlias(),
                PackageManager.COMPONENT_ENABLED_STATE_ENABLED,
                PackageManager.DONT_KILL_APP);
        } catch (Exception ignored) {}

        // Locked-firmware fallback: if WRITE_SECURE_SETTINGS was granted
        // (`pm grant com.moviango.mediaplayer android.permission.WRITE_
        // SECURE_SETTINGS`), self-enable the HOME-key accessibility
        // service — pressing HOME then brings Movian to the front
        // regardless of the OEM-pinned stock launcher. The picker chain
        // below still runs so the official route is used where it works.
        String svc = ctx.getPackageName() + "/" +
            ctx.getPackageName() + ".HomeKeyService";
        try {
            String cur = Settings.Secure.getString(
                ctx.getContentResolver(), "enabled_accessibility_services");
            if (cur == null || !cur.contains(svc)) {
                cur = (cur == null || cur.isEmpty()) ? svc : cur + ":" + svc;
                Settings.Secure.putString(ctx.getContentResolver(),
                    "enabled_accessibility_services", cur);
            }
            Settings.Secure.putInt(ctx.getContentResolver(),
                "accessibility_enabled", 1);
        } catch (SecurityException ignored) {
            // WRITE_SECURE_SETTINGS not granted — pickers still run.
        } catch (Exception ignored) {}

        // API 29+: the official "set as default" role dialog. Used via
        // reflection — our compile SDK predates android.app.role.
        if (Build.VERSION.SDK_INT >= 29) {
            try {
                Object rm = ctx.getSystemService("role");
                Class<?> rmc = Class.forName("android.app.role.RoleManager");
                if (Boolean.TRUE.equals(rmc
                        .getMethod("isRoleAvailable", String.class)
                        .invoke(rm, "android.app.role.HOME"))) {
                    Intent ri = (Intent) rmc
                        .getMethod("createRequestRoleIntent", String.class)
                        .invoke(rm, "android.app.role.HOME");
                    ri.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                    ctx.startActivity(ri);
                    return;
                }
            } catch (Exception ignored) {}
        }

        // The stock "Home app" settings page — the canonical picker on
        // standard Android. Skipped when the intent resolves to an OEM
        // do-nothing stub (class name contains "Stub": EmptyStubActivity
        // on TIM, Stubs$SettingsStub variants elsewhere).
        try {
            Intent i = new Intent(Settings.ACTION_HOME_SETTINGS);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            ResolveInfo ri = ctx.getPackageManager().resolveActivity(i, 0);
            boolean stub = ri == null || ri.activityInfo == null
                || ri.activityInfo.name == null
                || ri.activityInfo.name.contains("Stub");
            if (!stub) {
                ctx.startActivity(i);
                return;
            }
        } catch (Exception ignored) {}

        // Accessibility settings — manual path to enable HomeKeyService
        // when it couldn't self-enable (no WRITE_SECURE_SETTINGS grant)
        // and the firmware otherwise permits it. Skipped for OEM stubs.
        try {
            String en = Settings.Secure.getString(ctx.getContentResolver(),
                "enabled_accessibility_services");
            if (en == null || !en.contains(svc)) {
                Intent i = new Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS);
                i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                ResolveInfo ri = ctx.getPackageManager().resolveActivity(i, 0);
                boolean stub = ri == null || ri.activityInfo == null
                    || ri.activityInfo.name == null
                    || ri.activityInfo.name.contains("Stub");
                if (!stub) {
                    ctx.startActivity(i);
                    return;
                }
            }
        } catch (Exception ignored) {}

        // Chooser next: a bare HOME intent resolves straight to the
        // recorded default when one exists (e.g. the OEM-pinned TIM
        // launcher) without ever showing a picker. createChooser forces
        // the "Always / Just once" chooser over the HOME resolve set.
        // If even the service could not be enabled and every settings
        // page is stubbed, tell the user the one remaining step instead
        // of silently landing on the stock launcher.
        try {
            String en = Settings.Secure.getString(ctx.getContentResolver(),
                "enabled_accessibility_services");
            if (en == null || !en.contains(svc)) {
                android.widget.Toast.makeText(ctx,
                    "Launcher mode needs the accessibility service enabled.\n" +
                    "Via adb: pm grant " + ctx.getPackageName() +
                    " android.permission.WRITE_SECURE_SETTINGS,\n" +
                    "then press this button again.",
                    android.widget.Toast.LENGTH_LONG).show();
            }
        } catch (Exception ignored) {}
        try {
            Intent i = new Intent(Intent.ACTION_MAIN);
            i.addCategory(Intent.CATEGORY_HOME);
            i.addCategory(Intent.CATEGORY_DEFAULT);
            Intent chooser = Intent.createChooser(i, null);
            chooser.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            ctx.startActivity(chooser);
            return;
        } catch (Exception ignored) {}
        try {
            Intent i = new Intent(Intent.ACTION_MAIN);
            i.addCategory(Intent.CATEGORY_HOME);
            i.addCategory(Intent.CATEGORY_DEFAULT);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            ctx.startActivity(i);
            return;
        } catch (Exception ignored) {}
        try {
            Intent i = new Intent(Settings.ACTION_HOME_SETTINGS);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            ctx.startActivity(i);
        } catch (Exception e) {
            Log.w("Apps", "openHomeSettings failed", e);
        }
    }
}
