package ninja.jeremy.liveninja.ui.jobs

import androidx.lifecycle.ViewModel
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject

@HiltViewModel
class JobsNavigationViewModel @Inject constructor(inbox: JobsProposalInbox) : ViewModel() {
    val pending = inbox.pending
}
