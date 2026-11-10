// Page: account security — update admin credentials; all sessions are revoked afterwards.
import { api } from "../api.js";
import { t } from "../i18n.js";
import { $, toast } from "../ui.js";

export default {
  id: "account",
  title: "Bảo mật tài khoản",
  sub: "Cập nhật tài khoản quản trị; mọi phiên sẽ bị thu hồi.",
  init() {
    $("#account-form").addEventListener("submit", async (event) => {
      event.preventDefault();
      try {
        await api("/api/account", {
          method: "PUT",
          body: JSON.stringify({
            current_password: $("#a-current").value,
            username: $("#a-user").value,
            new_password: $("#a-new").value,
          }),
        });
        toast(t("Đã cập nhật tài khoản, vui lòng đăng nhập lại", "toast.account.updated"));
        setTimeout(() => location.reload(), 800);
      } catch (error) {
        toast(error.message, true);
      }
    });
  },
};
