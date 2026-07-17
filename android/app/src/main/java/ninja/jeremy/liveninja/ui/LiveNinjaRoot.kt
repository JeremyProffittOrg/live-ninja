package ninja.jeremy.liveninja.ui

import androidx.annotation.StringRes
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.outlined.FolderOpen
import androidx.compose.material.icons.outlined.Mic
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.res.stringResource
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.assistant.AssistTrigger
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.ui.onboarding.AuthViewModel
import ninja.jeremy.liveninja.ui.onboarding.LoginScreen
import ninja.jeremy.liveninja.ui.onboarding.OnboardingScreen
import ninja.jeremy.liveninja.ui.screens.ConversationScreen
import ninja.jeremy.liveninja.ui.screens.FilesScreen
import ninja.jeremy.liveninja.ui.screens.SettingsScreen

/** Top-level bottom-nav destinations. */
enum class TopLevelDestination(
    val route: String,
    @StringRes val labelRes: Int,
    val selectedIcon: ImageVector,
    val unselectedIcon: ImageVector,
) {
    CONVERSATION(
        route = "conversation",
        labelRes = R.string.destination_conversation,
        selectedIcon = Icons.Filled.Mic,
        unselectedIcon = Icons.Outlined.Mic,
    ),
    FILES(
        route = "files",
        labelRes = R.string.destination_files,
        selectedIcon = Icons.Filled.Folder,
        unselectedIcon = Icons.Outlined.FolderOpen,
    ),
    SETTINGS(
        route = "settings",
        labelRes = R.string.destination_settings,
        selectedIcon = Icons.Filled.Settings,
        unselectedIcon = Icons.Outlined.Settings,
    ),
}

/**
 * @param assistTriggers assistant activations ([ninja.jeremy.liveninja.assistant.AssistantEvents.triggers]);
 *   each new trigger navigates to the Conversation tab, where the realtime
 *   layer picks up the same trigger to start the session. Defaults to an empty
 *   flow for previews/tests.
 */
@Composable
fun LiveNinjaRoot(assistTriggers: SharedFlow<AssistTrigger> = MutableSharedFlow()) {
    // Root gate: first run -> onboarding wizard (which includes its sign-in
    // step via SignInLauncher); afterwards, a lost/expired session -> the
    // standalone login screen; otherwise the main scaffold.
    val authViewModel: AuthViewModel = hiltViewModel()
    val authState by authViewModel.authState.collectAsStateWithLifecycle()
    val onboardingCompleted by authViewModel.onboardingCompleted.collectAsStateWithLifecycle()
    if (!onboardingCompleted) {
        OnboardingScreen(onFinished = authViewModel::onOnboardingFinished)
        return
    }
    if (authState !is AuthState.SignedIn) {
        LoginScreen(viewModel = authViewModel)
        return
    }

    val navController = rememberNavController()
    val backStackEntry by navController.currentBackStackEntryAsState()
    val currentDestination = backStackEntry?.destination

    // The flow replays its latest trigger (so pre-composition launches are not
    // lost); the saved timestamp keeps a replay from re-navigating after
    // rotation/process restore.
    var lastHandledTrigger by rememberSaveable { mutableLongStateOf(0L) }
    LaunchedEffect(assistTriggers) {
        assistTriggers.collect { trigger ->
            if (trigger.timestampMillis <= lastHandledTrigger) return@collect
            lastHandledTrigger = trigger.timestampMillis
            navController.navigate(TopLevelDestination.CONVERSATION.route) {
                popUpTo(navController.graph.findStartDestination().id) { saveState = true }
                launchSingleTop = true
                restoreState = true
            }
        }
    }

    Scaffold(
        bottomBar = {
            NavigationBar {
                TopLevelDestination.entries.forEach { destination ->
                    val label = stringResource(destination.labelRes)
                    val selected = currentDestination?.hierarchy
                        ?.any { it.route == destination.route } == true
                    NavigationBarItem(
                        selected = selected,
                        onClick = {
                            navController.navigate(destination.route) {
                                popUpTo(navController.graph.findStartDestination().id) {
                                    saveState = true
                                }
                                launchSingleTop = true
                                restoreState = true
                            }
                        },
                        icon = {
                            Icon(
                                imageVector = if (selected) {
                                    destination.selectedIcon
                                } else {
                                    destination.unselectedIcon
                                },
                                contentDescription = null, // label below announces it
                            )
                        },
                        label = { Text(label) },
                    )
                }
            }
        },
    ) { innerPadding ->
        NavHost(
            navController = navController,
            startDestination = TopLevelDestination.CONVERSATION.route,
            modifier = Modifier.padding(innerPadding),
        ) {
            composable(TopLevelDestination.CONVERSATION.route) { ConversationScreen() }
            composable(TopLevelDestination.FILES.route) { FilesScreen() }
            composable(TopLevelDestination.SETTINGS.route) { SettingsScreen() }
        }
    }
}
